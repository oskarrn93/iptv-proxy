package iptvproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxReconnects = 3

// feedService holds one feed's business logic: which upstream URLs are safe
// to fetch, how playlists get rewritten so the upstream URL (and any Xtream
// credentials embedded in it) never reaches a client, and how to serve
// playlists, VOD segments, and live TS streams. It fetches upstream content
// through an upstreamRepository rather than an *http.Client directly.
type feedService struct {
	target  *url.URL
	timeout time.Duration
	// creds is this feed's own username/password pre-encoded as a query
	// fragment. It is stamped onto every rewritten playlist line, so it is
	// built once per feed rather than once per line.
	creds   string
	repo    *upstreamRepository
	logger  *slog.Logger
	metrics *feedMetrics
}

func newFeedService(cfg FeedConfig, repo *upstreamRepository, logger *slog.Logger, metrics *feedMetrics) (*feedService, error) {
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

	return &feedService{
		target:  target,
		timeout: cfg.Timeout,
		creds: fmt.Sprintf("username=%s&password=%s",
			url.QueryEscape(cfg.Username), url.QueryEscape(cfg.Password)),
		repo:    repo,
		logger:  logger,
		metrics: metrics,
	}, nil
}

// playlistTarget is the feed's own top-level playlist (the configured
// upstream_url).
func (s *feedService) playlistTarget() *url.URL {
	return s.target
}

// resolveStreamTarget parses the "u" query param of a /stream request into an
// absolute URL, without yet judging whether it's safe to fetch.
func (s *feedService) resolveStreamTarget(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() {
		return nil, errors.New("bad u param")
	}
	return target, nil
}

// parseStreamRequest parses and validates a /stream request's raw "u" param
// into a StreamRequest, with no knowledge of HTTP at all — handleStream is
// the only place that translates the result (or its error) into a response.
func (s *feedService) parseStreamRequest(ctx context.Context, raw string) (StreamRequest, error) {
	if raw == "" {
		return StreamRequest{}, &StreamRequestError{Err: errors.New("missing u")}
	}
	target, err := s.resolveStreamTarget(raw)
	if err != nil {
		return StreamRequest{}, &StreamRequestError{Err: err}
	}
	if err := s.validateStreamTarget(ctx, target); err != nil {
		return StreamRequest{}, &StreamRequestError{Forbidden: true, Err: err}
	}
	return StreamRequest{Target: target}, nil
}

// validateStreamTarget guards the /stream?u= relay. This container runs in the
// vpn container's network namespace, so without a check any client holding
// feed credentials could use it to read arbitrary LAN URLs. The configured
// upstream host is always allowed; anything else must resolve to a public
// address.
func (s *feedService) validateStreamTarget(ctx context.Context, target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", target.Scheme)
	}
	if strings.EqualFold(target.Host, s.target.Host) {
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
func (s *feedService) upstreamContext(reqCtx context.Context, target *url.URL) (context.Context, context.CancelFunc) {
	if isTSPath(target) {
		return reqCtx, func() {}
	}
	return context.WithTimeout(reqCtx, s.timeout)
}

// rewritePlaylist rewrites every segment/sub-playlist/EXT-X-KEY/EXT-X-MAP URI
// in the playlist into a link back through this feed's own /stream endpoint,
// carrying the proxy's own credentials so the upstream URL never leaks.
func (s *feedService) rewritePlaylist(body []byte, srcURL *url.URL) (string, error) {
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
			out.WriteString(s.rewriteAttrURI(trimmed, srcURL))
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			out.WriteString(line)
		default:
			resolved, err := resolveURI(srcURL, trimmed)
			if err != nil {
				out.WriteString(line)
				break
			}
			s.writeStreamURI(&out, resolved)
		}
		out.WriteByte('\n')
	}
	return out.String(), scanner.Err()
}

// writeStreamURI writes the /stream link that stands in for one upstream URI.
// Explicit writes rather than fmt.Fprintf: this runs once per playlist line, and
// a full channel list is tens of thousands of lines.
func (s *feedService) writeStreamURI(out *strings.Builder, resolved *url.URL) {
	out.WriteString("/stream?u=")
	out.WriteString(url.QueryEscape(resolved.String()))
	out.WriteByte('&')
	out.WriteString(s.creds)
}

// attrURIPattern matches one quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. Written as an alternation over the two quote styles rather
// than with a backreference, which Go's RE2 engine does not support.
var attrURIPattern = regexp.MustCompile(`(?i)URI=(?:"[^"]*"|'[^']*')`)

// rewriteAttrURI rewrites every quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. ReplaceAllStringFunc scans the original line left to right
// and never re-examines what it substitutes in, so a replacement that itself
// begins with `URI=` cannot be re-wrapped.
func (s *feedService) rewriteAttrURI(line string, base *url.URL) string {
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
		s.writeStreamURI(&out, resolved)
		out.WriteByte(delim)
		return out.String()
	})
}

// serve fetches target through the repository and writes the appropriate
// response to w: a rewritten playlist, a copied VOD segment, or a
// reconnecting live TS relay.
func (s *feedService) serve(reqCtx context.Context, w http.ResponseWriter, reqHeader http.Header, target *url.URL) {
	logger := loggerFromContext(reqCtx, s.logger)
	tctx, cancel := s.upstreamContext(reqCtx, target)
	defer cancel()
	resp, err := s.repo.fetchWithRetry(tctx, target.String(), reqHeader)
	if err != nil {
		// err wraps the full upstream URL, Xtream credentials included, so it
		// is logged (redacted) rather than returned to the client.
		logger.ErrorContext(reqCtx, "upstream fetch failed", "error", redactErr(err))
		s.metrics.recordUpstreamError(errorKindFetchFailed)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// An expired subscription or an exhausted retry budget arrives as a 4xx/5xx
	// body. Forwarding the status keeps the player from treating a rewritten
	// error page as a valid, empty playlist.
	if resp.StatusCode >= 400 {
		logger.WarnContext(reqCtx, "upstream returned error status", "status", resp.StatusCode, "target", redact(target.String()))
		s.metrics.recordUpstreamError(errorKindHTTPError)
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), resp.StatusCode)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	buf := bufio.NewReader(resp.Body)
	peeked, _ := buf.Peek(512)

	if shouldRewritePlaylist(target, contentType, peeked) {
		body, err := io.ReadAll(buf)
		if err != nil {
			logger.ErrorContext(reqCtx, "playlist read failed", "error", redactErr(err))
			http.Error(w, "read failed", http.StatusBadGateway)
			return
		}
		rewritten, err := s.rewritePlaylist(body, target)
		if err != nil {
			logger.ErrorContext(reqCtx, "playlist rewrite failed", "error", err)
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
		if err := s.streamWithReconnect(reqCtx, w, target, readCloserWrapper{Reader: buf, Closer: resp.Body}); err != nil {
			logger.ErrorContext(reqCtx, "raw TS reconnect error", "error", redactErr(err))
		}
		return
	}

	if _, err := bufferedCopy(w, buf); err != nil {
		logger.ErrorContext(reqCtx, "proxy copy failed", "error", redactErr(err))
	}
}

// streamWithReconnect relays a live TS stream, reopening the upstream when it
// drops. A clean EOF counts against the reconnect budget just as an error
// does: an upstream that has ended returns EOF immediately, and without
// counting it this loop would refetch without pause forever. The budget resets
// whenever a connection actually delivered data, so a stream that survives for
// hours can still reconnect more than maxReconnects times in total.
func (s *feedService) streamWithReconnect(ctx context.Context, w http.ResponseWriter, target *url.URL, initialBody io.ReadCloser) error {
	logger := loggerFromContext(ctx, s.logger)
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
			logger.WarnContext(ctx, "raw TS disconnected, reconnecting", "attempt", attempts, "max_attempts", maxReconnects, "error", redactErr(err))
		} else {
			logger.InfoContext(ctx, "raw TS upstream ended, reconnecting", "bytes_written", written, "attempt", attempts, "max_attempts", maxReconnects)
		}

		currentBody.Close()
		currentBody = nil
		s.metrics.streamReconnects.Inc()
		wait = sleepWithBackoff(wait)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		resp, err := s.repo.fetchWithRetry(ctx, target.String(), nil)
		if err != nil {
			return err
		}
		currentBody = resp.Body
	}
}
