package iptvproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxReconnects = 3

// playlistGroupKey is the singleflight.Group key for a feed's top-level
// playlist fetch. A feedService has exactly one playlist target (its
// configured upstream_url), so every call coalesces under this one constant
// key rather than one derived per-request.
const playlistGroupKey = "playlist"

// playlistCacheTTL is how long a successfully fetched playlist is served from
// memory before the next request triggers a fresh upstream fetch. IPTV apps
// poll the playlist every few seconds but a channel list changes rarely, so
// this trades staleness (up to the TTL) for cutting upstream load from "once
// per poll" to "once per TTL" per feed.
const playlistCacheTTL = 3 * time.Hour

// epgGroupKey is the singleflight.Group key for a feed's EPG fetch, mirroring
// playlistGroupKey: a feedService has exactly one EPG target (its upstream's
// xmltv.php), so every call coalesces under this one constant key.
const epgGroupKey = "epg"

// epgCacheTTL is how long a fetched XMLTV guide is served from memory before
// the next request triggers a fresh upstream fetch. An EPG guide is tens of
// megabytes and changes far less often than a channel list, so this is much
// longer than playlistCacheTTL.
const epgCacheTTL = 12 * time.Hour

// maxPlaylistGrow caps the up-front allocation rewritePlaylist sizes from the
// upstream's Content-Length.
const maxPlaylistGrow = 8 << 20

// feedService holds one feed's business logic: which upstream URLs are safe
// to fetch, how playlists get rewritten so the upstream URL (and any Xtream
// credentials embedded in it) never reaches a client, and how to serve
// playlists, VOD segments, and live TS streams. It fetches upstream content
// through an upstreamRepository rather than an *http.Client directly.
type feedService struct {
	// name is the feed's configured name, used to label its logs and to say
	// which feed a rejected request was attributed to.
	name    string
	target  *url.URL
	timeout time.Duration
	// excludeGroupPrefixes is cfg.ExcludeGroupPrefixes lowercased once here
	// rather than per playlist line (see FeedConfig.ExcludeGroupPrefixes).
	excludeGroupPrefixes []string
	// hideCredentials swaps the upstream's own credentials out of a URL for
	// this feed's; restoreCredentials swaps them back. Together they are what
	// lets a rewritten link stand in for an upstream URL without carrying it:
	// the link holds the feed's credentials in the exact spots the upstream's
	// occupied, and streamTarget puts the upstream's back before fetching.
	hideCredentials    credentialSwap
	restoreCredentials credentialSwap
	// creds are this feed's own configured credentials, written into every
	// rewritten link (see writeStreamURI) and matched against an incoming
	// /stream request to find the feed again.
	creds Credentials
	// upstreamCreds are the credentials this feed's own upstream_url embeds
	// (zero if it embeds none), used to build the EPG target (see epgTarget)
	// — the one place besides hideCredentials/restoreCredentials that needs
	// the upstream's own pair rather than this feed's.
	upstreamCreds Credentials
	repo          *upstreamRepository
	logger        *slog.Logger
	metrics       *feedMetrics
	// clientBufferDelay configures the live-TS delay buffer (see
	// streamLive/delayedReader): nil disables it, otherwise its value is how
	// long a byte is held before reaching the client. Proxy-wide
	// (Config.ClientBuffer), not per-feed, but carried on feedService since
	// that's what serve/streamLive run on.
	clientBufferDelay *time.Duration
	// playlistCache coalesces concurrent requests for this feed's playlist
	// into a single upstream fetch — a channel list can be tens of megabytes,
	// and IPTV apps routinely poll it every few seconds from multiple
	// devices, so without this every viewer multiplies upstream load — and
	// serves the result for playlistCacheTTL before the next request
	// triggers a fresh fetch.
	playlistCache *singleflightCache[playlistFetchResult]
	// epgCache mirrors playlistCache for the EPG route, coalescing concurrent
	// fetches of the same multi-megabyte XMLTV guide and caching the result
	// for epgCacheTTL.
	epgCache *singleflightCache[epgFetchResult]
	// liveTargets tracks which upstream URLs currently have an open live TS
	// relay (see serveWith's live branch), so a second concurrent request for
	// the exact same channel — whether a normal /stream viewer or a /share
	// link pointing at it — is rejected instead of opening a second upstream
	// connection and silently burning another of the account's (usually
	// limited) concurrent-connection slots.
	liveTargets *liveTargetTracker
}

// liveTargetTracker records which upstream target URLs currently have an
// open live TS relay. It exists only for this one-live-stream-per-url
// guarantee, kept separate from feedMetrics.sessionStarted/sessionEnded
// (which counts every /stream request, including short segment/playlist
// fetches) since only the live TS relay actually holds a single upstream
// connection open for the viewer's whole session.
type liveTargetTracker struct {
	mu      sync.Mutex
	targets map[string]struct{}
}

func newLiveTargetTracker() *liveTargetTracker {
	return &liveTargetTracker{targets: make(map[string]struct{})}
}

// acquire claims target for the caller and reports true, or reports false if
// another relay already holds it.
func (t *liveTargetTracker) acquire(target string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, busy := t.targets[target]; busy {
		return false
	}
	t.targets[target] = struct{}{}
	return true
}

// release frees target so a later request for the same channel can proceed.
func (t *liveTargetTracker) release(target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.targets, target)
}

// playlistFetchResult is the outcome of one upstream playlist fetch, shared
// verbatim across every request coalesced onto it by feedService.playlistCache.
type playlistFetchResult struct {
	body   string
	status int
}

// playlistFetchError carries the HTTP status a failed playlist fetch should
// be reported with, so servePlaylist can translate it without re-deriving
// which failure mode it came from.
type playlistFetchError struct {
	status int
	msg    string
}

func (e *playlistFetchError) Error() string { return e.msg }

// epgFetchResult is the outcome of one upstream XMLTV fetch, shared verbatim
// across every request coalesced onto it by feedService.epgCache. Unlike a playlist, an
// EPG body is opaque to this proxy — nothing in it needs rewriting, since an
// XMLTV guide carries channel ids/titles, not upstream stream URLs — so it is
// cached and served as-is, with only its upstream Content-Type carried along.
type epgFetchResult struct {
	body        []byte
	status      int
	contentType string
}

func newFeedService(cfg FeedConfig, repo *upstreamRepository, logger *slog.Logger, metrics *feedMetrics, clientBuffer ClientBufferConfig) (*feedService, error) {
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
	// An Xtream upstream carries its own credentials as get.php query params
	// and repeats them in every stream path it hands out, so the configured
	// upstream_url is all it takes to know what to swap out of a rewritten
	// link — and back in when the request for it arrives. An upstream with no
	// such credentials simply has nothing to swap; its links then carry the
	// whole upstream path instead (see streamURIPathParts).
	upstreamCreds := credentialsFromURL(target)

	// A credential that a path spells differently than the config does would
	// never match where it has to (see Credentials.unsafePathField): this
	// feed's own would leave links no client could authenticate against, and
	// the upstream's would stay in those links rather than being swapped out.
	// Caught at startup instead of as unplayable channels or a quiet leak.
	for _, pair := range []struct {
		scope string
		creds Credentials
	}{
		{"", cfg.Credentials},
		{"upstream_url ", upstreamCreds},
	} {
		if field := pair.creds.unsafePathField(); field != "" {
			return nil, fmt.Errorf("%s%s must be URL-path-safe: it has to survive a round trip through a stream link's path", pair.scope, field)
		}
	}

	// The two swaps are exact mirrors of each other, so only one is spelled
	// out: deriving the other states that relationship rather than leaving a
	// reader to verify the field assignments line up.
	hide := credentialSwap{from: upstreamCreds, to: cfg.Credentials}

	var clientBufferDelay *time.Duration
	if clientBuffer.IsEnabled() {
		clientBufferDelay = &clientBuffer.Duration
	}

	return &feedService{
		name:                 cfg.Name,
		target:               target,
		timeout:              cfg.Timeout,
		excludeGroupPrefixes: lowercaseAll(cfg.ExcludeGroupPrefixes),
		hideCredentials:      hide,
		restoreCredentials:   hide.reversed(),
		creds:                cfg.Credentials,
		upstreamCreds:        upstreamCreds,
		repo:                 repo,
		logger:               logger,
		metrics:              metrics,
		playlistCache:        newSingleflightCache[playlistFetchResult](playlistCacheTTL, playlistGroupKey),
		epgCache:             newSingleflightCache[epgFetchResult](epgCacheTTL, epgGroupKey),
		clientBufferDelay:    clientBufferDelay,
		liveTargets:          newLiveTargetTracker(),
	}, nil
}

// matches reports whether creds are exactly this feed's own. The service that
// writes credentials into rewritten links is the one that recognizes them
// coming back, so the two can't drift apart.
func (s *feedService) matches(creds Credentials) (userOK, passOK bool) {
	return s.creds.matches(creds)
}

// streamTarget rebuilds the upstream URL a rewritten stream link stands for:
// this feed's configured upstream scheme and host, with the link's path and
// query put back the way writeStreamURI found them. The host comes from config
// and never from the request, so a /stream request can only ever reach the
// configured upstream — which is why nothing here has to check that the target
// isn't a LAN address.
func (s *feedService) streamTarget(prefix, rest, rawQuery string) *url.URL {
	path, query := s.restoreCredentials.apply(streamURIPath(prefix, s.creds, rest), rawQuery)
	return &url.URL{Scheme: s.target.Scheme, Host: s.target.Host, Path: path, RawQuery: query}
}

// epgTarget is this feed's upstream XMLTV guide URL: the same scheme/host as
// its configured upstream_url (never anything a client supplies, same
// reasoning as streamTarget), with the standard Xtream /xmltv.php path and
// this feed's own upstream credentials as query params. A feed whose
// upstream_url embeds no credentials (upstreamCreds is zero) hits xmltv.php
// with no query at all, which is what an upstream with no such scheme expects.
func (s *feedService) epgTarget() *url.URL {
	target := &url.URL{Scheme: s.target.Scheme, Host: s.target.Host, Path: "/xmltv.php"}
	if s.upstreamCreds.Username != "" || s.upstreamCreds.Password != "" {
		q := url.Values{"username": {s.upstreamCreds.Username}, "password": {s.upstreamCreds.Password}}
		target.RawQuery = q.Encode()
	}
	return target
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

// groupTitlePattern extracts an #EXTINF line's group-title="..." attribute,
// which is the only signal an Xtream m3u_plus channel list gives for which
// category (live channel group, "Movies: <genre>", "Series: <show>") an
// entry belongs to.
var groupTitlePattern = regexp.MustCompile(`(?i)group-title="([^"]*)"`)

// excludeEntry reports whether an #EXTINF line's group-title matches one of
// this feed's configured excludeGroupPrefixes (see FeedConfig.ExcludeGroupPrefixes),
// e.g. filtering "Movies:"/"Series:" VOD entries out of a channel list.
func (s *feedService) excludeEntry(extinfLine string) bool {
	if len(s.excludeGroupPrefixes) == 0 {
		return false
	}
	m := groupTitlePattern.FindStringSubmatch(extinfLine)
	if m == nil {
		return false
	}
	group := strings.ToLower(m[1])
	for _, prefix := range s.excludeGroupPrefixes {
		if strings.HasPrefix(group, prefix) {
			return true
		}
	}
	return false
}

// uriWriter writes the link that stands in for one resolved upstream URI into
// a playlist being rewritten. writeStreamURI is the default (Xtream-shaped
// feed links); serveShare passes shareURIWriter instead, so the same rewrite
// pass can back either a normal /stream link or a token-scoped /share one
// without duplicating rewritePlaylist/rewriteAttrURI/serve for the latter.
type uriWriter func(out *strings.Builder, resolved *url.URL)

// rewritePlaylist rewrites every segment/sub-playlist/EXT-X-KEY/EXT-X-MAP URI
// in the playlist into a link back through this proxy (see writeURI) — so
// neither the upstream host nor the credentials it embeds reach the client.
// It also drops any #EXTINF entry (and its following URI line) whose
// group-title matches excludeGroupPrefixes.
// The reader is consumed strictly line by line, so a full channel list never
// has to be held as one []byte — sizeHint is the upstream's Content-Length, or
// <= 0 when it arrived chunked.
func (s *feedService) rewritePlaylist(r io.Reader, srcURL *url.URL, sizeHint int64, writeURI uriWriter) (string, error) {
	scanner := bufio.NewScanner(r)
	// Open at 64 KB and let the scanner grow only if a line really needs it; the
	// 1 MB is the safety cap, not the opening allocation.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var out strings.Builder
	// The rewrite is roughly size-preserving, so sizing once up front beats
	// letting the Builder double its way up to a multi-megabyte playlist. The
	// hint is capped because excludeGroupPrefixes can drop the great majority
	// of an Xtream m3u_plus list, and a 40 MB upstream that rewrites down to
	// 2 MB shouldn't open with a 50 MB allocation.
	if sizeHint > 0 {
		grow := sizeHint + sizeHint/4
		if grow > maxPlaylistGrow {
			grow = maxPlaylistGrow
		}
		out.Grow(int(grow))
	}

	// skipURI is set by an excluded #EXTINF line so the URI line that follows
	// it (the entry's actual stream target) is dropped too, rather than left
	// as an orphaned link with no preceding #EXTINF.
	skipURI := false

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "#EXTINF"):
			if s.excludeEntry(trimmed) {
				skipURI = true
				continue
			}
			skipURI = false
			out.WriteString(line)
		case strings.HasPrefix(trimmed, "#EXT-X-KEY") || strings.HasPrefix(trimmed, "#EXT-X-MAP"):
			out.WriteString(rewriteAttrURI(trimmed, srcURL, writeURI))
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			out.WriteString(line)
		default:
			if skipURI {
				skipURI = false
				continue
			}
			resolved, err := resolveURI(srcURL, trimmed)
			if err != nil {
				out.WriteString(line)
				break
			}
			writeURI(&out, resolved)
		}
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	// strings.Clone, not out.String(): the returned string aliases the
	// Builder's backing array, so without a copy sized to the actual output the
	// whole grown capacity stays reachable — and a cached playlist pins it for
	// playlistCacheTTL.
	return strings.Clone(out.String()), nil
}

// baseURLPlaceholder stands in for the client-facing scheme://host in every
// link writeStreamURI writes into a cached playlist. The real host a client
// reached this proxy through is only known per-request (see
// requestBaseURL), while a playlist is fetched/rewritten once and shared
// across every client and host it's cached for — so the actual host is
// substituted in at response time (applyBaseURL) rather than baked in here.
// Chosen to contain characters (`\x00`) that can never appear in an upstream
// playlist body, so ReplaceAll can never match anything but this placeholder.
const baseURLPlaceholder = "\x00BASEURL\x00"

// applyBaseURL substitutes the requesting client's own scheme://host (see
// requestBaseURL) for every baseURLPlaceholder a cached/rewritten playlist
// carries, making its links absolute for that client without needing the
// playlist rewritten (or cached separately) per host.
func applyBaseURL(body, baseURL string) string {
	return strings.ReplaceAll(body, baseURLPlaceholder, baseURL)
}

// writeStreamURI writes the link that stands in for one upstream URI, shaped
// like a standard Xtream stream URL — /<prefix>/<username>/<password>/<file>
// — so IPTV clients that parse or rely on that convention (rather than
// treating the M3U entry as an opaque URL) keep working. The username and
// password are this feed's own configured credentials, standing exactly where
// the upstream's stood, so a client sees a plausible stream URL while the
// upstream host and the upstream's own credentials never appear in it. The
// host portion is written as baseURLPlaceholder, filled in per-request by
// applyBaseURL. handleStream reverses this to recover the upstream URL — see
// streamTarget.
//
// Only credentials the config knows about can be swapped out, which for an
// Xtream feed is exactly the pair its upstream_url carries and repeats in
// every stream path. An upstream that hands out paths bearing some other
// secret would leak it, having given this proxy no way to recognize it.
//
// Explicit writes rather than fmt.Fprintf: this runs once per playlist line,
// and a full channel list is tens of thousands of lines.
func (s *feedService) writeStreamURI(out *strings.Builder, resolved *url.URL) {
	path, query := s.hideCredentials.apply(resolved.EscapedPath(), resolved.RawQuery)
	prefix, rest := streamURIPathParts(path, s.creds)
	out.WriteString(baseURLPlaceholder)
	out.WriteByte('/')
	out.WriteString(prefix)
	out.WriteByte('/')
	out.WriteString(s.creds.Username)
	out.WriteByte('/')
	out.WriteString(s.creds.Password)
	out.WriteByte('/')
	out.WriteString(rest)
	if query != "" {
		out.WriteByte('?')
		out.WriteString(query)
	}
}

// fallbackStreamPrefix is the prefix used for an upstream path that is not
// shaped like an Xtream stream URL, marking that the whole path was carried in
// the link's trailing segments instead. streamURIPathParts never emits it as a
// real prefix, so streamURIPath can tell the two forms apart with certainty.
const fallbackStreamPrefix = "stream"

// noTypeStreamPrefix marks a path shaped like /<username>/<password>/<file...>
// — credentials right at the start, with no leading type segment (some
// upstreams omit "live"/"movie"/etc. for live channels). Without a distinct
// marker for this shape it would fall through to fallbackStreamPrefix, whose
// reconstruction carries rest verbatim — but rest here is exactly
// "<username>/<password>/<file>", so writeStreamURI's explicit
// username/password segments would duplicate the ones already inside rest.
// streamURIPathParts never emits it as a real prefix, so streamURIPath can
// tell this shape apart from both the standard and fallback ones.
const noTypeStreamPrefix = "stream-no-type"

// streamURIPathParts splits a credential-swapped upstream path — this feed's
// username/password already standing where the upstream's did — into the
// prefix and trailing segments of the link writeStreamURI emits. A standard
// Xtream stream path is /<prefix>/<username>/<password>/<file...> (e.g.
// /live/u/p/12345.ts, /series/u/p/3/1/98765.mkv,
// /timeshift/u/p/60/2024-01-01:00-00/12345.ts); when the path has that shape
// the prefix and the segments after the credentials are reused verbatim, so
// the link is indistinguishable in structure from a real Xtream one and
// streamURIPath can put the credentials back where they came from. Some
// upstreams instead omit the type segment entirely for live channels —
// /<username>/<password>/<file...>, credentials at the very start — which
// this also recognizes so its credentials aren't left in rest to be
// duplicated by writeStreamURI's explicit username/password segments. Every
// other shape — a path with the credentials elsewhere or not at all — falls
// back to carrying the whole path after the credentials instead.
func streamURIPathParts(path string, creds Credentials) (prefix, rest string) {
	trimmed := strings.Trim(path, "/")
	// SplitN(..., 4) caps the allocation and leaves anything past the 3rd
	// slash as one unsplit segment, so no strings.Join is needed to rebuild it.
	segments := strings.SplitN(trimmed, "/", 4)
	if len(segments) >= 3 && segments[0] != fallbackStreamPrefix && segments[0] != noTypeStreamPrefix &&
		segments[1] == creds.Username && segments[2] == creds.Password {
		if len(segments) == 4 {
			rest = segments[3]
		}
		return segments[0], rest
	}

	segments3 := strings.SplitN(trimmed, "/", 3)
	if len(segments3) >= 2 && segments3[0] == creds.Username && segments3[1] == creds.Password {
		if len(segments3) == 3 {
			rest = segments3[2]
		}
		return noTypeStreamPrefix, rest
	}

	return fallbackStreamPrefix, trimmed
}

// vodStreamPrefixes are the Xtream path prefixes streamURIPathParts emits for
// on-demand content (movie/series), as opposed to a live channel. Used to
// classify a /stream request's sessionKind from its prefix alone, before
// anything is fetched from upstream.
var vodStreamPrefixes = map[string]bool{"movie": true, "series": true}

// classifySessionKind reports what kind of /stream request prefix identifies,
// for the activeSessions gauge. noTypeStreamPrefix and the standard "live"
// prefix are both live channels; movie/series are VOD; anything else
// (timeshift, catch-up, or a non-standard upstream shape via
// fallbackStreamPrefix) is bucketed as "other" rather than guessed at.
func classifySessionKind(prefix string) sessionKind {
	switch {
	case prefix == "live" || prefix == noTypeStreamPrefix:
		return sessionKindLive
	case vodStreamPrefixes[prefix]:
		return sessionKindVOD
	default:
		return sessionKindOther
	}
}

// streamURIPath rejoins what streamURIPathParts split, returning the upstream
// path with this feed's credentials still standing in for the upstream's (see
// streamTarget, which swaps them back).
func streamURIPath(prefix string, creds Credentials, rest string) string {
	switch prefix {
	case fallbackStreamPrefix:
		return "/" + rest
	case noTypeStreamPrefix:
		return "/" + creds.Username + "/" + creds.Password + "/" + rest
	default:
		return "/" + prefix + "/" + creds.Username + "/" + creds.Password + "/" + rest
	}
}

// attrURIPattern matches one quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. Written as an alternation over the two quote styles rather
// than with a backreference, which Go's RE2 engine does not support.
var attrURIPattern = regexp.MustCompile(`(?i)URI=(?:"[^"]*"|'[^']*')`)

// rewriteAttrURI rewrites every quoted URI= attribute on an #EXT-X-KEY /
// #EXT-X-MAP line. ReplaceAllStringFunc scans the original line left to right
// and never re-examines what it substitutes in, so a replacement that itself
// begins with `URI=` cannot be re-wrapped.
func rewriteAttrURI(line string, base *url.URL, writeURI uriWriter) string {
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
		writeURI(&out, resolved)
		out.WriteByte(delim)
		return out.String()
	})
}

// fetchPlaylist fetches and rewrites this feed's top-level playlist. It is
// only ever called from inside playlistGroup.Do, so it must not write to any
// particular caller's http.ResponseWriter — its result is handed out to every
// request that coalesced onto the same call. For the same reason it fetches
// with a context detached from any single caller: a coalesced leader whose
// own client disconnects must not cancel the fetch for the followers sharing
// its result.
func (s *feedService) fetchPlaylist(reqHeader http.Header) (playlistFetchResult, error) {
	tctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	resp, err := s.repo.fetchWithRetry(tctx, s.target.String(), reqHeader)
	if err != nil {
		return playlistFetchResult{}, s.upstreamFailure(tctx, s.target, err)
	}
	defer resp.Body.Close()

	if pfErr := s.upstreamStatusFailure(tctx, s.target, resp); pfErr != nil {
		return playlistFetchResult{}, pfErr
	}
	return s.readAndRewrite(tctx, resp.Body, s.target, resp.StatusCode, resp.ContentLength, s.writeStreamURI)
}

// upstreamFailure logs a failed upstream fetch, counts it, and returns the
// error the caller should report it as. err wraps the full upstream URL,
// Xtream credentials included, so it is logged (redacted) rather than returned
// to the client.
func (s *feedService) upstreamFailure(ctx context.Context, target *url.URL, err error) *playlistFetchError {
	loggerFromContext(ctx, s.logger).ErrorContext(ctx, "upstream fetch failed", "error", redactErr(err))
	s.metrics.recordUpstreamError(errorKindFetchFailed)
	return &playlistFetchError{status: http.StatusBadGateway, msg: "upstream fetch failed"}
}

// upstreamStatusFailure reports an upstream 4xx/5xx, or nil if the response is
// one to serve. An expired subscription or an exhausted retry budget arrives
// as an error body; forwarding the status keeps the player from treating a
// rewritten error page as a valid, empty playlist.
func (s *feedService) upstreamStatusFailure(ctx context.Context, target *url.URL, resp *http.Response) *playlistFetchError {
	if resp.StatusCode < 400 {
		return nil
	}
	loggerFromContext(ctx, s.logger).WarnContext(ctx, "upstream returned error status",
		"status", resp.StatusCode, "target", redact(target.String()))
	s.metrics.recordUpstreamError(errorKindHTTPError)
	return &playlistFetchError{status: resp.StatusCode, msg: fmt.Sprintf("upstream returned %d", resp.StatusCode)}
}

// readAndRewrite turns an upstream playlist body into the result to serve. It
// is the one place that decides how a read or rewrite failure is reported, so
// the top-level playlist and a sub-playlist reached through /stream — which
// are the same operation — can't drift apart.
func (s *feedService) readAndRewrite(ctx context.Context, body io.Reader, target *url.URL, status int, sizeHint int64, writeURI uriWriter) (playlistFetchResult, error) {
	logger := loggerFromContext(ctx, s.logger)
	rewritten, err := s.rewritePlaylist(body, target, sizeHint, writeURI)
	if err != nil {
		// Scanning is the only thing that can fail here, and it fails only on
		// a bad read of the upstream body — hence 502 rather than a 500 for a
		// rewrite this proxy got wrong.
		logger.ErrorContext(ctx, "playlist read failed", "error", redactErr(err))
		return playlistFetchResult{}, &playlistFetchError{status: http.StatusBadGateway, msg: "read failed"}
	}
	return playlistFetchResult{body: rewritten, status: status}, nil
}

// fetchEPG fetches this feed's XMLTV guide from upstream. Only ever called
// from inside epgGroup.Do, for the same reason fetchPlaylist is: its result is
// shared across every request coalesced onto the same call, so it fetches
// with a context detached from any single caller rather than the request that
// happened to trigger it.
func (s *feedService) fetchEPG() (epgFetchResult, error) {
	tctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	target := s.epgTarget()
	resp, err := s.repo.fetchWithRetry(tctx, target.String(), nil)
	if err != nil {
		return epgFetchResult{}, s.upstreamFailure(tctx, target, err)
	}
	defer resp.Body.Close()

	if pfErr := s.upstreamStatusFailure(tctx, target, resp); pfErr != nil {
		return epgFetchResult{}, pfErr
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		loggerFromContext(tctx, s.logger).ErrorContext(tctx, "epg read failed", "error", redactErr(err))
		return epgFetchResult{}, &playlistFetchError{status: http.StatusBadGateway, msg: "read failed"}
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/xml"
	}
	return epgFetchResult{body: body, status: resp.StatusCode, contentType: contentType}, nil
}

// writeEPGResponse writes a fetched (or cached) EPG result to w verbatim — an
// XMLTV guide carries no upstream URLs, so unlike a playlist it needs no
// per-request host substitution.
func writeEPGResponse(w http.ResponseWriter, result epgFetchResult) {
	w.Header().Set("Content-Type", result.contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(result.status)
	_, _ = w.Write(result.body)
}

// serveEPG serves this feed's XMLTV guide, following the same
// cache-then-coalesce shape as servePlaylist: a request within epgCacheTTL of
// the last successful upstream fetch is served straight from that cache;
// otherwise it coalesces onto a single upstream fetch via epgGroup, shared
// with any other request racing it for the same refresh.
func (s *feedService) serveEPG(reqCtx context.Context, w http.ResponseWriter) {
	logger := loggerFromContext(reqCtx, s.logger)

	if result, ok := s.epgCache.get(); ok {
		logger.InfoContext(reqCtx, "epg fetch", "cached", true)
		writeEPGResponse(w, result)
		return
	}

	result, shared, err := s.epgCache.fetch(s.fetchEPG)
	logger.InfoContext(reqCtx, "epg fetch", "cached", false, "coalesced", shared)

	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	s.epgCache.store(result)
	writeEPGResponse(w, result)
}

// writeUpstreamError reports a failed upstream request with the status the
// failure carries, falling back to 502 for an error from somewhere other than
// the fetch pipeline. Shared by the playlist and stream routes so an upstream
// failure reads the same whichever one hit it.
func writeUpstreamError(w http.ResponseWriter, err error) {
	status, msg := http.StatusBadGateway, "upstream fetch failed"
	var pfErr *playlistFetchError
	if errors.As(err, &pfErr) {
		status, msg = pfErr.status, pfErr.msg
	}
	http.Error(w, msg, status)
}

// writePlaylistResponse writes a fetched (or cached) playlist result to w,
// with baseURLPlaceholder resolved to this particular request's own
// scheme://host (see requestBaseURL) so its links are absolute for whichever
// host the client actually reached this proxy through.
func writePlaylistResponse(w http.ResponseWriter, result playlistFetchResult, baseURL string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(result.status)
	io.WriteString(w, applyBaseURL(result.body, baseURL))
}

// servePlaylist serves this feed's top-level playlist. A request within
// playlistCacheTTL of the last successful upstream fetch is served straight
// from that cache; otherwise it coalesces onto a single upstream fetch via
// playlistGroup, shared with any other request racing it for the same
// refresh. shared reports whether this particular call rode along on another
// in-flight fetch rather than triggering its own.
func (s *feedService) servePlaylist(reqCtx context.Context, w http.ResponseWriter, reqHeader http.Header, baseURL string) {
	result, err := s.getPlaylist(reqCtx, reqHeader)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writePlaylistResponse(w, result, baseURL)
}

// getPlaylist returns this feed's own top-level playlist, cached-or-fetched
// the same way servePlaylist always has — pulled out on its own so the admin
// page's channel listing (see admin.go) can read the very same rewritten
// playlist a client sees, instead of talking to the upstream a second way.
func (s *feedService) getPlaylist(reqCtx context.Context, reqHeader http.Header) (playlistFetchResult, error) {
	logger := loggerFromContext(reqCtx, s.logger)

	if result, ok := s.playlistCache.get(); ok {
		logger.InfoContext(reqCtx, "playlist fetch", "cached", true)
		return result, nil
	}

	result, shared, err := s.playlistCache.fetch(func() (playlistFetchResult, error) {
		return s.fetchPlaylist(reqHeader)
	})
	logger.InfoContext(reqCtx, "playlist fetch", "cached", false, "coalesced", shared)

	if err != nil {
		return playlistFetchResult{}, err
	}

	s.playlistCache.store(result)
	return result, nil
}

// serve fetches target through the repository and writes the appropriate
// response to w: a rewritten playlist, a copied VOD segment, or a
// reconnecting live TS relay. Nested playlist URIs are rewritten as normal
// /stream links (see writeStreamURI).
func (s *feedService) serve(reqCtx context.Context, w http.ResponseWriter, reqHeader http.Header, target *url.URL, baseURL string) {
	s.serveWith(reqCtx, w, reqHeader, target, baseURL, s.writeStreamURI)
}

// serveWith is serve with the playlist-rewrite link strategy pulled out as a
// parameter, so serveShare can reuse the exact same fetch/rewrite/live-relay
// logic while writing nested playlist URIs as /share/<token> links instead of
// this feed's own /stream ones.
func (s *feedService) serveWith(reqCtx context.Context, w http.ResponseWriter, reqHeader http.Header, target *url.URL, baseURL string, writeURI uriWriter) {
	logger := loggerFromContext(reqCtx, s.logger)
	tctx, cancel := s.upstreamContext(reqCtx, target)
	defer cancel()
	resp, err := s.repo.fetchWithRetry(tctx, target.String(), reqHeader)
	if err != nil {
		// context.Canceled here means the viewer gave up (closed the
		// connection, or an app bulk-validating the whole channel list moved
		// on) before the upstream responded — not an upstream failure, so it
		// shouldn't count toward upstream error metrics or alert-worthy logs.
		if errors.Is(err, context.Canceled) {
			logger.InfoContext(reqCtx, "stream request canceled: client disconnected")
			return
		}
		writeUpstreamError(w, s.upstreamFailure(reqCtx, target, err))
		return
	}
	defer resp.Body.Close()

	if pfErr := s.upstreamStatusFailure(reqCtx, target, resp); pfErr != nil {
		writeUpstreamError(w, pfErr)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	buf := bufio.NewReader(resp.Body)
	peeked, _ := buf.Peek(512)

	if shouldRewritePlaylist(target, contentType, peeked) {
		result, err := s.readAndRewrite(reqCtx, buf, target, resp.StatusCode, resp.ContentLength, writeURI)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writePlaylistResponse(w, result, baseURL)
		return
	}

	// A body of known length is finite — an HLS segment or a ranged VOD chunk —
	// so it is copied once. Only an unbounded body (chunked/EOF-delimited) is a
	// live stream worth reconnecting to.
	isTS := isTSStream(target, contentType, peeked)
	live := isTS && resp.ContentLength < 0

	// A live relay holds one upstream connection open for the viewer's whole
	// session, unlike a playlist poll or a single segment fetch — so this is
	// the one place worth enforcing "only one stream per upstream url": a
	// second concurrent request for the same channel (another direct viewer,
	// or a /share link pointing at it) would otherwise open a second upstream
	// connection for what the provider sees as the same channel. Checked
	// before any header is written, since the response is a 409 if the target
	// is already busy.
	if live {
		liveKey := target.String()
		if !s.liveTargets.acquire(liveKey) {
			logger.WarnContext(reqCtx, "rejecting duplicate live stream request", "target", redact(liveKey))
			http.Error(w, "channel already being streamed", http.StatusConflict)
			return
		}
		defer s.liveTargets.release(liveKey)
	}

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
		if err := s.streamLive(reqCtx, w, target, readCloserWrapper{Reader: buf, Closer: resp.Body}); err != nil {
			// context.Canceled here means the viewer stopped playback (client
			// closed the connection), not that the upstream or reconnect logic
			// failed — logging it as an error would page/alert on normal
			// viewer behavior.
			if errors.Is(err, context.Canceled) {
				logger.InfoContext(reqCtx, "raw TS stream ended: client disconnected")
			} else {
				logger.ErrorContext(reqCtx, "raw TS reconnect error", "error", redactErr(err))
			}
		}
		return
	}

	if _, err := bufferedCopy(w, buf, nil); err != nil {
		logger.ErrorContext(reqCtx, "proxy copy failed", "error", redactErr(err))
	}
}

// streamWithReconnect relays a live TS stream, reopening the upstream when it
// drops. A clean EOF counts against the reconnect budget just as an error
// does: an upstream that has ended returns EOF immediately, and without
// counting it this loop would refetch without pause forever. The budget resets
// whenever a connection actually delivered data, so a stream that survives for
// hours can still reconnect more than maxReconnects times in total.
//
// The reconnect+backoff gap would otherwise leave the client's socket
// completely idle — no bytes, no error — which reads to a player as a stall
// (spinner, or the player timing out the connection) rather than a brief
// glitch. Immediately replaying the last chunk that was actually flushed to
// the client bridges that gap: the player decodes it as a very brief freeze
// instead, and keeps reading from what still looks like a live connection
// while the upstream reconnect happens.
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
	var lastChunk []byte

	for {
		written, err := bufferedCopy(w, currentBody, &lastChunk)
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

		if len(lastChunk) > 0 {
			if _, werr := w.Write(lastChunk); werr != nil {
				return werr
			}
			if ferr := http.NewResponseController(w).Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return ferr
			}
		}

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
