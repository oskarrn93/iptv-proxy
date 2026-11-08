# CLAUDE.md

This file provides guidance to Claude Code when working with code in this directory.

## What this is

`iptv-proxy` hosts multiple upstream M3U8/Xtream IPTV feeds behind unguessable per-feed credentials, all sharing one listen address/port. A request's own HTTP Basic username/password (not which port it hit) picks which feed's upstream serves it. It rewrites every playlist/segment URL in the upstream M3U8 so the real upstream (and any Xtream credentials embedded in it) never reach the client. It also proxies the Xtream Codes JSON API (`player_api.php`/`panel_api.php`) itself, not just the M3U playlist — see "Xtream Codes API proxying" below.

Go module: `github.com/oskarrn93/iptv-proxy` (Go 1.27). Deployed in the `media-services` homelab stack — see that repo for the compose/VPN/Traefik context this service runs inside (it's one of the `iptv-proxy-{1..3}` services network-namespaced through `vpn`).

For the shape of the Xtream Codes API this proxy forwards (actions, request/response fields), the `xtream_code_client` Dart package's docs are a useful reference even though this codebase isn't Dart: https://pub.dev/documentation/xtream_code_client/latest/ — it documents `player_api.php` actions (`get_live_categories`, `get_live_streams`, `get_vod_categories`, `get_vod_streams`, `get_series`, `get_series_info`, `get_vod_info`, and the bare-query `authenticate`/login call returning `user_info`/`server_info`), the `get.php` M3U playlist endpoint, and `xmltv.php` EPG endpoints.

## HLS/M3U8 protocol compliance

Playlist parsing and rewriting (`internal/iptvproxy/service.go`: `rewritePlaylist`, `rewriteAttrURI`, `writeStreamURI`) must stay conformant with **RFC 8216 (HTTP Live Streaming)**: https://www.rfc-editor.org/info/rfc8216/

Key implications when touching this code:

- Every URI line and URI-valued attribute in a playlist must be treated as a reference that may need rewriting — not just segment lines. That includes `EXT-X-KEY`, `EXT-X-MAP`, `EXT-X-MEDIA` (`URI=`), `EXT-X-I-FRAME-STREAM-INF`, and nested/variant playlist URIs, in addition to plain segment/sub-playlist lines. Anything missed leaks the upstream origin (and any embedded Xtream credentials) to the client.
- URIs in a playlist may be relative to the playlist's own URL, not the feed's base URL — resolve with `resolveURI` (base = the fetched playlist's URL) per RFC 8216 §4.1, not by assuming a fixed prefix.
- Tags and attribute lists are line-oriented and order-sensitive (e.g. `EXT-X-STREAM-INF` immediately precedes the URI line it describes) — don't reorder or drop lines while rewriting, only rewrite the URI portions.
- Master playlists (variant streams) vs. media playlists (segments) are distinguished by tag content, not file extension or path — `EXT-X-STREAM-INF` marks a master playlist; don't assume every `.m3u8` is a media playlist.
- Content sniffing (`shouldRewritePlaylist`, `isTSStream` in `util.go`) exists because upstreams don't always report accurate `Content-Type`; preserve that fallback rather than trusting headers alone.
- When adding support for new EXT-X tags, check the tag's URI/attribute semantics against RFC 8216 §4.3 before deciding whether it needs rewriting.

## Commands

```sh
go run ./cmd/iptv-proxy -config config.yaml   # run locally (config.yaml is gitignored; copy config.yaml.example)
go build ./...
go test ./...
go vet ./...
```

`internal/iptvproxy/admin.html`'s feed/channel combobox JS has its own Playwright regression suite (real Chromium, real admin.html, no live server needed — see `internal/iptvproxy/admin_ui_test/README.md`):

```sh
cd internal/iptvproxy/admin_ui_test && npm ci && npx playwright install chromium && npm test
```

## Architecture notes

- `internal/iptvproxy/config.go` — loads and validates `config.yaml` (feeds, per-feed credentials, listen addr).
- `internal/iptvproxy/handler.go` — HTTP routing, per-request auth (`authenticate`/`matchFeed`), request logging/instrumentation.
- `internal/iptvproxy/service.go` — per-feed logic: playlist fetch + rewrite, stream proxying/reconnect, playlist caching.
- `internal/iptvproxy/credentials.go` — feed credential matching.
- `internal/iptvproxy/trust.go` — `trusted_proxies`/`admin_allowed_cidrs`: `stripUntrustedForwarded` (run by `withRequestLogger` on every app route) drops `X-Forwarded-*` from peers outside `trusted_proxies`, and `adminOnly` wraps every `/admin/*` route so it's served only to requests whose TCP peer is a trusted proxy *and* whose original client (rightmost non-proxy `X-Forwarded-For` hop) is in `admin_allowed_cidrs`. Login itself is Traefik/tinyauth's job; this is the in-app backstop that still holds if that routing breaks (it once did: the admin router was bound only to `web`, so `/admin` on `:443` fell through to the public router with no login). Empty lists fail closed. Every new `/admin/*` route must be wrapped in `adminOnly`.
- `internal/iptvproxy/shares.go` — temporary share links. Nested URIs in a share's rewritten playlist are `/share/<token>?ref=<sealed>`, where the ref is the upstream URL encrypted with that share's own AES-GCM key (`share.sealRef`/`openRef`). Never write a raw upstream URL into a share link: for Xtream upstreams it contains the real upstream username/password in its path (`TestHandleShare_PlaylistDoesNotLeakUpstream`).
- `internal/iptvproxy/util.go` — URI resolution, content sniffing, buffered copy, backoff, per-request base URL detection (`requestBaseURL`).
- `internal/iptvproxy/metrics.go` — Prometheus metrics (feed-scoped counters/histograms).
- No `WriteTimeout` on the HTTP server (`cmd/iptv-proxy/main.go`) — a live TS relay writes for as long as the viewer watches; don't add one.
- Playlist cache (`singleflightCache.getOrFetch`) exists as both a concurrent-viewer optimization and a stale-fallback source if the upstream fetch fails — preserve stale-fallback behavior when changing fetch logic.
- Live TS relaying (the `live` branch of `feedService.serve`) goes through `streamLive` (`delaybuffer.go`), which — when `Config.ClientBuffer` is enabled (default: on, 5s) — holds every byte read from upstream for `client_buffer.duration` before it reaches the client (`delayedReader`), smoothing brief upstream stalls/reconnects out of the client's view at the cost of that same playback-start delay. It does this by pointing `streamWithReconnect` (unchanged, including its own reconnect/gap-bridging logic) at an in-memory `io.Pipe` instead of the real `http.ResponseWriter`, via `pipeResponseWriter`, then draining that pipe through the delay buffer into the real client. Disabled, `streamLive` is exactly `streamWithReconnect`. This is proxy-wide config (`Config.ClientBuffer`), not per-feed — see `config.yaml.example`.
- `/epg` (`handleEPG` in `handler.go`, `feedService.serveEPG`/`fetchEPG`/`epgTarget` in `service.go`) proxies the matched feed's XMLTV guide from its upstream's `/xmltv.php` (same host as `upstream_url`, upstream's own credentials as query params — see `epgTarget`), cached for `epgCacheTTL` (12h, much longer than `playlistCacheTTL` since a guide changes far less often) with the same cache-then-singleflight-coalesce shape as the playlist route. Unlike a playlist, the XMLTV body is opaque to this proxy — nothing in it is a stream URL, so it's cached and served byte-for-byte with no rewriting.
- Rewritten `/stream` links carry `baseURLPlaceholder` (a sentinel, not a real host) instead of a configured `public_base_url` — there is no such config. The cached/rewritten playlist body is host-agnostic; `applyBaseURL` substitutes in the actual `scheme://host` per request (from `requestBaseURL`: `X-Forwarded-Host`/`X-Forwarded-Proto` if present — which, after `stripUntrustedForwarded`, means only if a trusted proxy sent them — else `Host`/TLS) in `writePlaylistResponse`. This is what lets one cached playlist serve correct absolute links to clients reaching the proxy through different hostnames (LAN vs. external) without re-fetching or re-rewriting per host.

## Xtream Codes API proxying

`/player_api.php` and its `/panel_api.php` alias (`handleXtreamAPI` in `handler.go`, `feedService.serveXtreamAPI`/`xtreamAPITarget`/`rewriteXtreamAPIBody` in `xtream.go`) let an Xtream Codes IPTV app log in and browse categories/streams/VOD/series against this proxy exactly as it would against the real upstream, on top of the same feeds the M3U routes already serve — there is no separate `protocol:` config field. A feed's Xtream upstream login is inferred from `upstream_url` the same way the M3U routes already do: `s.target` (scheme/host) plus `s.upstreamCreds` (username/password parsed from `upstream_url`'s own query params via `credentialsFromURL`).

Key behaviors to preserve when touching `xtream.go`:

- Unlike `/playlist.m3u8`/`/epg`, this route is **not cached**: a `player_api.php` request carries arbitrary query params (`action`, `category_id`, `vod_id`, `series_id`, ...), so there's no single stable target to coalesce concurrent requests onto the way `servePlaylist`/`serveEPG` do for their one fixed upstream URL. Every request is a fresh upstream fetch.
- `xtreamAPITarget` reuses `s.restoreCredentials` (the same swap `streamTarget` uses to put the upstream's credentials back) to turn the client's own query string into the upstream's — so the credential-matching semantics for a `player_api.php` request are exactly the same as for a rewritten stream link, not a second implementation of the same idea.
- The JSON response is walked recursively (`rewriteXtreamValue`) and rewritten in three ways — extend this walk, don't bypass it, when a new field needs rewriting:
  - a nested `user_info` object's `username`/`password` (matched by key name, not by response shape/action) are swapped from the upstream's real pair back to this feed's own, via `rewriteUserInfo`.
  - a nested `server_info` object's `url`/`port`/`https_port`/`server_protocol` are pointed at this proxy (via `rewriteServerInfo`, using the same `requestBaseURL`-derived `baseURL` as the playlist route) rather than the upstream, so a client that builds its own stream URLs from `server_info` instead of using links this proxy already rewrote still reaches back through this proxy.
  - any string leaf that parses as an absolute URL on this feed's own upstream host (e.g. a VOD/series entry's `direct_source`) is rewritten through `writeStreamURI` — the identical `/<prefix>/<username>/<password>/<file>` link shape a playlist entry gets — via `rewriteXtreamURLString`.
- A response body that fails to parse as JSON (an upstream error page, a misconfigured upstream) is passed through unrewritten rather than dropped — see `rewriteXtreamAPIBody`'s fallback.
- `maxXtreamAPIBody` caps how much of a response this proxy will buffer to `json.Unmarshal` — unlike the playlist/stream routes, which stream line-by-line or copy bytes without holding the whole body, rewriting JSON structurally requires the full body in memory first.
