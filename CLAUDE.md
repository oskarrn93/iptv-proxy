# CLAUDE.md

This file provides guidance to Claude Code when working with code in this directory.

## What this is

`iptv-proxy` hosts multiple upstream M3U8/Xtream IPTV feeds behind unguessable per-feed credentials, all sharing one listen address/port. A request's own HTTP Basic username/password (not which port it hit) picks which feed's upstream serves it. It rewrites every playlist/segment URL in the upstream M3U8 so the real upstream (and any Xtream credentials embedded in it) never reach the client.

Go module: `github.com/oskarrn93/media-services/iptv-proxy` (Go 1.27). Part of the `media-services` homelab stack — see `../.claude/CLAUDE.md` for the compose/VPN/Traefik context this service runs inside (it's one of the `iptv-proxy-{1..4}` services network-namespaced through `vpn`).

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

## Architecture notes

- `internal/iptvproxy/config.go` — loads and validates `config.yaml` (feeds, per-feed credentials, listen addr).
- `internal/iptvproxy/handler.go` — HTTP routing, per-request auth (`authenticate`/`matchFeed`), request logging/instrumentation.
- `internal/iptvproxy/service.go` — per-feed logic: playlist fetch + rewrite, stream proxying/reconnect, playlist caching.
- `internal/iptvproxy/credentials.go` — feed credential matching.
- `internal/iptvproxy/util.go` — URI resolution, content sniffing, buffered copy, backoff.
- `internal/iptvproxy/metrics.go` — Prometheus metrics (feed-scoped counters/histograms).
- No `WriteTimeout` on the HTTP server (`cmd/iptv-proxy/main.go`) — a live TS relay writes for as long as the viewer watches; don't add one.
- Playlist cache (`cachedPlaylist`/`storePlaylistCache`) exists as both a concurrent-viewer optimization and a stale-fallback source if the upstream fetch fails — preserve stale-fallback behavior when changing fetch logic.
