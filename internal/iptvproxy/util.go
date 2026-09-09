package iptvproxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	mrand "math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	bufferSize    = 32 * 1024
	retryDelay    = 250 * time.Millisecond
	maxRetryDelay = 2 * time.Second
)

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

// requestBaseURL derives the scheme://host a client actually reached this
// proxy at, from the request alone — so a rewritten playlist's links can be
// made absolute without any configured public_base_url. X-Forwarded-Host/
// X-Forwarded-Proto (set by a reverse proxy such as Traefik) take priority
// over Host/TLS so a proxied request reports the address the client used,
// not this process's own listen_addr; a request with several comma-separated
// values in either header (multiple proxy hops) uses the first, which is the
// one closest to the original client. This is what lets the same cached
// playlist serve LAN and external clients alike, each seeing links back to
// whichever host they themselves connected through.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := firstForwarded(r.Header.Get("X-Forwarded-Proto")); fwd != "" {
		scheme = fwd
	}

	return scheme + "://" + requestHost(r)
}

// requestHost derives the host a client actually reached this proxy at,
// e.g. "iptv.oskarrosen.io" or "192.168.1.12:8080" — the same
// X-Forwarded-Host-over-Host precedence as requestBaseURL, but without the
// scheme, for attaching to logs/metrics as "which entry point was this
// request made through" (LAN vs. external).
func requestHost(r *http.Request) string {
	if fwd := firstForwarded(r.Header.Get("X-Forwarded-Host")); fwd != "" {
		return fwd
	}
	return r.Host
}

// firstForwarded returns the first comma-separated value in an
// X-Forwarded-* header, trimmed, or "" if the header was empty.
func firstForwarded(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// boolPtr returns a pointer to b, for building a *bool config field (e.g.
// ClientBufferConfig.Enabled) inline where Go doesn't allow &false directly.
func boolPtr(b bool) *bool { return &b }

// lowercaseAll lowercases each string in ss, so a case-insensitive prefix
// list only needs its comparisons done once at config-load time rather than
// once per playlist line.
func lowercaseAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
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
// bytes reached the client. The count lets the reconnect loop tell a stream
// that ran for a while from one that returns EOF immediately.
//
// lastChunk, if non-nil, is overwritten with a copy of the most recent chunk
// written to w — letting a caller replay it after the read loop ends, to
// bridge a reconnect gap instead of leaving the client with silence. Pass
// nil when the caller has no use for it (finite bodies aren't reconnected).
func bufferedCopy(w http.ResponseWriter, r io.Reader, lastChunk *[]byte) (int64, error) {
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
			if lastChunk != nil {
				*lastChunk = append((*lastChunk)[:0], buf[:n]...)
			}
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

// newRequestID returns a short unguessable id for tagging every log line
// produced while handling one request, so they can be grepped/grouped
// together (e.g. "request_id=a1b2c3d4e5f6a7b8").
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

type readCloserWrapper struct {
	io.Reader
	io.Closer
}

// sleepWithBackoff sleeps for wait plus jitter, then returns the next (doubled)
// wait — both capped at maxRetryDelay. Returning the next value keeps the
// doubling visible at the call site instead of hiding it behind a pointer.
// Shared by the repository's fetch retry loop and the service's stream
// reconnect loop.
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
