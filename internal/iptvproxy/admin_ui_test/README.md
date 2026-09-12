# admin_ui_test

Playwright regression tests for `../admin.html`'s feed/channel combobox
JavaScript, run against the real file in a real Chromium — see
`admin.test.js`'s file comment for what specifically these guard against and
why (the pre-select-without-firing-`change` bug, and the focus-re-filters
-by-stale-text bug).

## Running

```sh
cd internal/iptvproxy/admin_ui_test
npm ci                       # first run only
npx playwright install chromium   # first run only, ~190MB
npm test
```

Also documented in `../../CLAUDE.md`.

## No CDN, no network

`admin.html` loads htmx/json-enc from `/admin/static/htmx.js` and
`/admin/static/json-enc.js` — vendored copies admin.go go:embeds from
`../static/` (see that directory), not a CDN. `server.js` serves those same
files for these tests, so there's no separate copy to fall out of sync and
no network access needed to run this suite. Bumping the vendored version
only requires updating `../static/` (see its own note on how) — nothing
here needs to change.
