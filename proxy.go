package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxRetries    = 3
	retryDelay    = 250 * time.Millisecond
	maxRetryDelay = 2 * time.Second
	bufferSize    = 32 * 1024
	maxReconnects = 3
)

// feedProxy proxies one upstream M3U8 feed, rewriting every playlist/segment
// URI to route back through this feed's own listen address so the upstream
// URL (and its embedded Xtream credentials) never reaches the client.
type feedProxy struct {
	name     string
	target   *url.URL
	username string
	password string
	timeout  time.Duration
	client   *http.Client
	// creds is this feed's own username/password pre-encoded as a query
	// fragment. It is stamped onto every rewritten playlist line, so it is
	// built once per feed rather than once per line.
	creds string
}

func newFeedProxy(cfg FeedConfig) (*feedProxy, error) {
	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream_url: %w", err)
	}
	// A URL without "//" parses as an opaque one — "host:8080/get.php" becomes
	// scheme "host" — so checking the scheme catches more operator typos than
	// IsAbs alone, and reports them at startup instead of on first fetch.
	if !target.IsAbs() || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, fmt.Errorf("invalid upstream_url %q: must be an absolute http:// or https:// URL", cfg.UpstreamURL)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

	return &feedProxy{
		name:     cfg.Name,
		target:   target,
		username: cfg.Username,
		password: cfg.Password,
		timeout:  cfg.Timeout,
		client:   &http.Client{Transport: transport},
		creds: fmt.Sprintf("username=%s&password=%s",
			url.QueryEscape(cfg.Username), url.QueryEscape(cfg.Password)),
	}, nil
}

func (p *feedProxy) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", p.handlePlaylist)
	mux.HandleFunc("/", p.handlePlaylist)
	mux.HandleFunc("/stream", p.handleStream)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// requireAuth accepts either ?username=&password= query params or HTTP Basic
// Auth so both "M3U URL" and "Xtream login" style IPTV apps work.
func (p *feedProxy) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	user := r.URL.Query().Get("username")
	pass := r.URL.Query().Get("password")
	if user == "" && pass == "" {
		if basicUser, basicPass, ok := r.BasicAuth(); ok {
			user, pass = basicUser, basicPass
		}
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(p.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(p.password)) == 1
	if userOK && passOK {
		return true
	}
	log.Printf("[%s] auth failed: %s %s from %s", p.name, r.Method, r.URL.Path, r.RemoteAddr)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// credsPattern matches the credential query params that Xtream upstream URLs
// carry, so they can be stripped before a URL (or an error wrapping one)
// reaches a client or the container log.
var credsPattern = regexp.MustCompile(`(?i)([?&](?:username|password|token|auth)=)[^&\s"']*`)

func redact(s string) string {
	return credsPattern.ReplaceAllString(s, "${1}REDACTED")
}

// redactErr renders err for logging with any upstream credentials removed —
// *url.Error embeds the full request URL, query string included.
func redactErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return redact(err.Error())
}

// forwardedRequestHeaders are copied from the client request to the upstream
// fetch. Range/If-Range matter for VOD seeking; the rest keep the upstream
// from serving something different than it would to the client directly.
var forwardedRequestHeaders = []string{"Range", "If-Range", "Accept", "User-Agent", "Referer"}

// forwardedResponseHeaders are copied back from the upstream response for
// non-playlist bodies so ranged/seekable responses stay intact.
var forwardedResponseHeaders = []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

func (p *feedProxy) fetchUpstream(ctx context.Context, rawURL string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range forwardedRequestHeaders {
		if v := hdr.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	return p.client.Do(req)
}

func isTransientError(err error) bool {
	// net.Error.Timeout rather than the deprecated Temporary ("Temporary errors
	// are not well-defined", Go 1.18). It covers the transport-level deadlines
	// set above, which never surface as context.DeadlineExceeded.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	return false
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (p *feedProxy) fetchUpstreamWithRetry(ctx context.Context, rawURL string, hdr http.Header) (*http.Response, error) {
	wait := retryDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := p.fetchUpstream(ctx, rawURL, hdr)
		if err == nil {
			if isRetryableStatus(resp.StatusCode) {
				if attempt == maxRetries {
					return resp, nil
				}
				resp.Body.Close()
				log.Printf("[%s] upstream retryable response %d, retrying after %s", p.name, resp.StatusCode, wait)
				wait = sleepWithBackoff(wait)
				continue
			}
			return resp, nil
		}
		if !isTransientError(err) || attempt == maxRetries {
			return nil, err
		}
		log.Printf("[%s] upstream fetch failed, retrying (%d/%d) after %s: %s", p.name, attempt+1, maxRetries, wait, redactErr(err))
		wait = sleepWithBackoff(wait)
	}
	return nil, errors.New("upstream retry loop exited unexpectedly")
}

// sleepWithBackoff sleeps for wait plus jitter, then returns the next (doubled)
// wait — both capped at maxRetryDelay. Returning the next value keeps the
// doubling visible at the call site instead of hiding it behind a pointer.
func sleepWithBackoff(wait time.Duration) time.Duration {
	jitter := time.Duration(mrand.Int63n(int64(wait/2 + time.Millisecond)))
	delay := wait + jitter
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	time.Sleep(delay)
	if next := wait * 2; next < maxRetryDelay {
		return next
	}
	return maxRetryDelay
}

func resolveURI(base *url.URL, uri string) (*url.URL, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	return base.ResolveReference(u), nil
}

// shouldRewritePlaylist reports whether a response body is an HLS playlist and
// so needs its URIs rewritten. Checks run cheapest-first: path suffix, declared
// content type, then the peeked leading bytes — plenty of IPTV upstreams serve
// playlists as text/plain, so the body sniff is the backstop. target.Path is
// used rather than the whole URL: it already excludes the query and fragment
// that a suffix test would otherwise have to strip.
func shouldRewritePlaylist(target *url.URL, contentType string, peeked []byte) bool {
	if strings.HasSuffix(strings.ToLower(target.Path), ".m3u8") {
		return true
	}
	// "mpegurl" already subsumes the vnd.apple./x- spellings of the type.
	if strings.Contains(strings.ToLower(contentType), "mpegurl") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(peeked), []byte("#EXTM3U"))
}

// isTSPath reports whether a URL looks like raw MPEG-TS from its path alone —
// the only signal available before a response has arrived.
func isTSPath(target *url.URL) bool {
	path := strings.ToLower(target.Path)
	return strings.Contains(path, "/ts/stream") || strings.HasSuffix(path, ".ts")
}

// isTSStream adds the signals that exist only once the response is in hand: the
// declared content type, and the 0x47 sync byte every TS packet starts with.
func isTSStream(target *url.URL, contentType string, peeked []byte) bool {
	if isTSPath(target) {
		return true
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "video/mp2t") || strings.Contains(ct, "video/ts") {
		return true
	}
	return len(peeked) > 0 && peeked[0] == 0x47
}

// bufferedCopy streams r to w, flushing as it goes, and reports how many
// bytes reached the client. The count lets streamRawTSWithReconnect tell a
// stream that ran for a while from one that returns EOF immediately.
func bufferedCopy(w http.ResponseWriter, r io.Reader) (int64, error) {
	// ResponseController rather than a w.(http.Flusher) assertion: it unwraps a
	// wrapped ResponseWriter instead of silently never flushing, which on a live
	// stream would leave the client with nothing until a buffer filled. A writer
	// that genuinely cannot flush is not an error.
	rc := http.NewResponseController(w)
	buf := make([]byte, bufferSize)
	var written int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			nw, errw := w.Write(buf[:n])
			written += int64(nw)
			if errw != nil {
				return written, errw
			}
			if errf := rc.Flush(); errf != nil && !errors.Is(errf, http.ErrNotSupported) {
				return written, errf
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return written, nil
			}
			return written, err
		}
	}
}

// rewritePlaylist rewrites every segment/sub-playlist/EXT-X-KEY/EXT-X-MAP URI
// in the playlist into a link back through this feed's own /stream endpoint,
// carrying the proxy's own credentials so the upstream URL never leaks.
func (p *feedProxy) rewritePlaylist(body []byte, srcURL *url.URL) (string, error) {
	// bytes.NewReader, not strings.NewReader(string(body)): the latter copies the
	// whole playlist, and a full Xtream channel list runs to tens of megabytes.
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Open at 64 KB and let the scanner grow only if a line really needs it; the
	// 1 MB is the safety cap, not the opening allocation.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var out strings.Builder
	// The rewrite is roughly size-preserving, so sizing once up front beats
	// letting the Builder double its way up to a multi-megabyte playlist.
	out.Grow(len(body) + len(body)/4)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-KEY") || strings.HasPrefix(trimmed, "#EXT-X-MAP"):
			out.WriteString(p.rewriteAttrURI(trimmed, srcURL))
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			out.WriteString(line)
		default:
			resolved, err := resolveURI(srcURL, trimmed)
			if err != nil {
				out.WriteString(line)
				break
			}
			p.writeStreamURI(&out, resolved)
		}
		out.WriteByte('\n')
	}
	return out.String(), scanner.Err()
}

// writeStreamURI writes the /stream link that stands in for one upstream URI.
// Explicit writes rather than fmt.Fprintf: this runs once per playlist line, and
// a full channel list is tens of thousands of lines.
func (p *feedProxy) writeStreamURI(out *strings.Builder, resolved *url.URL) {
	out.WriteString("/stream?u=")
	out.WriteString(url.QueryEscape(resolved.String()))
	out.WriteByte('&')
	out.WriteString(p.creds)
}

// attrURIPattern matches one quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. Written as an alternation over the two quote styles rather
// than with a backreference, which Go's RE2 engine does not support.
var attrURIPattern = regexp.MustCompile(`(?i)URI=(?:"[^"]*"|'[^']*')`)

// rewriteAttrURI rewrites every quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. ReplaceAllStringFunc scans the original line left to right
// and never re-examines what it substitutes in, so a replacement that itself
// begins with `URI=` cannot be re-wrapped.
func (p *feedProxy) rewriteAttrURI(line string, base *url.URL) string {
	return attrURIPattern.ReplaceAllStringFunc(line, func(match string) string {
		// match is `URI=` + delimiter + value + the same delimiter.
		delim := match[len("URI=")]
		orig := match[len("URI=")+1 : len(match)-1]
		resolved, err := resolveURI(base, orig)
		if err != nil {
			return match
		}
		var out strings.Builder
		out.WriteString("URI=")
		out.WriteByte(delim)
		p.writeStreamURI(&out, resolved)
		out.WriteByte(delim)
		return out.String()
	})
}

// validateStreamTarget guards the /stream?u= relay. This container runs in the
// vpn container's network namespace, so without a check any client holding
// feed credentials could use it to read arbitrary LAN URLs. The configured
// upstream host is always allowed; anything else must resolve to a public
// address.
func (p *feedProxy) validateStreamTarget(ctx context.Context, target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", target.Scheme)
	}
	if strings.EqualFold(target.Host, p.target.Host) {
		return nil
	}
	host := target.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", host, err)
	}
	for _, addr := range addrs {
		ip := addr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("host %q resolves to non-public address %s", host, ip)
		}
	}
	return nil
}

// upstreamContext gives finite responses a deadline and leaves live TS streams
// without one. It runs before the fetch, so the path is the only signal
// available — hence isTSPath rather than the full isTSStream.
func (p *feedProxy) upstreamContext(reqCtx context.Context, target *url.URL) (context.Context, context.CancelFunc) {
	if isTSPath(target) {
		return reqCtx, func() {}
	}
	return context.WithTimeout(reqCtx, p.timeout)
}

// handlePlaylist serves the feed's own top-level playlist (the configured
// upstream_url). Clients point their IPTV app at this URL instead of the
// real upstream.
func (p *feedProxy) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	if !p.requireAuth(w, r) {
		return
	}
	log.Printf("[%s] playlist request: %s %s from %s", p.name, r.Method, r.URL.Path, r.RemoteAddr)

	p.proxyTarget(w, r, p.target)
}

// handleStream serves rewritten segment/sub-playlist links produced by
// rewritePlaylist, fetching whatever upstream URL is given in "u".
func (p *feedProxy) handleStream(w http.ResponseWriter, r *http.Request) {
	if !p.requireAuth(w, r) {
		return
	}
	raw := r.URL.Query().Get("u")
	if raw == "" {
		http.Error(w, "missing u", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() {
		http.Error(w, "bad u param", http.StatusBadRequest)
		return
	}
	if err := p.validateStreamTarget(r.Context(), target); err != nil {
		log.Printf("[%s] rejected stream target from %s: %v", p.name, r.RemoteAddr, err)
		http.Error(w, "forbidden target", http.StatusForbidden)
		return
	}

	p.proxyTarget(w, r, target)
}

func (p *feedProxy) proxyTarget(w http.ResponseWriter, r *http.Request, target *url.URL) {
	tctx, cancel := p.upstreamContext(r.Context(), target)
	defer cancel()
	resp, err := p.fetchUpstreamWithRetry(tctx, target.String(), r.Header)
	if err != nil {
		// err wraps the full upstream URL, Xtream credentials included, so it
		// is logged (redacted) rather than returned to the client.
		log.Printf("[%s] upstream fetch failed: %s", p.name, redactErr(err))
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// An expired subscription or an exhausted retry budget arrives as a 4xx/5xx
	// body. Forwarding the status keeps the player from treating a rewritten
	// error page as a valid, empty playlist.
	if resp.StatusCode >= 400 {
		log.Printf("[%s] upstream returned %d for %s", p.name, resp.StatusCode, redact(target.String()))
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), resp.StatusCode)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	buf := bufio.NewReader(resp.Body)
	peeked, _ := buf.Peek(512)

	if shouldRewritePlaylist(target, contentType, peeked) {
		body, err := io.ReadAll(buf)
		if err != nil {
			log.Printf("[%s] playlist read failed: %s", p.name, redactErr(err))
			http.Error(w, "read failed", http.StatusBadGateway)
			return
		}
		rewritten, err := p.rewritePlaylist(body, target)
		if err != nil {
			log.Printf("[%s] playlist rewrite failed: %v", p.name, err)
			http.Error(w, "rewrite failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		io.WriteString(w, rewritten)
		return
	}

	// A body of known length is finite — an HLS segment or a ranged VOD chunk —
	// so it is copied once. Only an unbounded body (chunked/EOF-delimited) is a
	// live stream worth reconnecting to.
	isTS := isTSStream(target, contentType, peeked)
	live := isTS && resp.ContentLength < 0

	if contentType == "" {
		if isTS {
			contentType = "video/MP2T"
		} else {
			contentType = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	if !live {
		for _, name := range forwardedResponseHeaders {
			if v := resp.Header.Get(name); v != "" {
				w.Header().Set(name, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)

	if live {
		if err := p.streamRawTSWithReconnect(r.Context(), w, target, readCloserWrapper{Reader: buf, Closer: resp.Body}); err != nil {
			log.Printf("[%s] raw TS reconnect error: %s", p.name, redactErr(err))
		}
		return
	}

	if _, err := bufferedCopy(w, buf); err != nil {
		log.Printf("[%s] proxy copy failed: %s", p.name, redactErr(err))
	}
}

type readCloserWrapper struct {
	io.Reader
	io.Closer
}

// streamRawTSWithReconnect relays a live TS stream, reopening the upstream
// when it drops. A clean EOF counts against the reconnect budget just as an
// error does: an upstream that has ended returns EOF immediately, and without
// counting it this loop would refetch without pause forever. The budget resets
// whenever a connection actually delivered data, so a stream that survives for
// hours can still reconnect more than maxReconnects times in total.
func (p *feedProxy) streamRawTSWithReconnect(ctx context.Context, w http.ResponseWriter, target *url.URL, initialBody io.ReadCloser) error {
	currentBody := initialBody
	defer func() {
		if currentBody != nil {
			currentBody.Close()
		}
	}()

	attempts := 0
	wait := retryDelay

	for {
		written, err := bufferedCopy(w, currentBody)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if written > 0 {
			attempts = 0
			wait = retryDelay
		}

		attempts++
		if attempts > maxReconnects {
			return err
		}
		if err != nil {
			log.Printf("[%s] raw TS disconnected, reconnect attempt %d/%d: %s", p.name, attempts, maxReconnects, redactErr(err))
		} else {
			log.Printf("[%s] raw TS upstream ended after %d bytes, reconnect attempt %d/%d", p.name, written, attempts, maxReconnects)
		}

		currentBody.Close()
		currentBody = nil
		wait = sleepWithBackoff(wait)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		resp, err := p.fetchUpstreamWithRetry(ctx, target.String(), nil)
		if err != nil {
			return err
		}
		currentBody = resp.Body
	}
}
