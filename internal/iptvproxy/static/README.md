# static

`admin.css` and `admin.js` are the admin page's own code. The rest is vendored
JS for `../admin.html`, go:embedded by `../admin.go`
(`adminStatic`) and served at `/admin/static/htmx.js` /
`/admin/static/json-enc.js` — kept local rather than fetched from a CDN so
the admin page needs no outbound internet access, like every other route in
this LAN-only proxy.

Currently pinned to **htmx 2.0.10**. To upgrade:

```sh
curl -o htmx.js https://unpkg.com/htmx.org@<version>
curl -o json-enc.js https://unpkg.com/htmx.org@<version>/dist/ext/json-enc.js
```

`../admin_ui_test/server.js` serves these same files for the Playwright
suite — nothing else needs to change there.
