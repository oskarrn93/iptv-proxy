// server.js is a minimal stand-in for the real iptv-proxy admin server
// (admin.go's handleAdminPage/handleAdminHTMX/handleAdminJSONEnc/
// handleAdminFeeds/handleAdminChannels/handleListShares), used only so
// Playwright can drive the REAL admin.html (../admin.html — the file
// admin.go go:embeds and serves byte-for-byte) over real HTTP in a real
// browser, without needing a running iptv-proxy instance or real IPTV
// feeds. /admin/static/htmx.js and /admin/static/json-enc.js are served
// from ../static/ — the exact files admin.go embeds and admin.html's
// <script src> tags point at — so there's no separate vendored copy for
// these tests to fall out of sync with. Each test supplies its own
// `routes` (see admin.test.js) to control what /admin/feeds,
// /admin/channels, and /admin/shares return.
"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");

const ADMIN_HTML_PATH = path.join(__dirname, "..", "admin.html");
const STATIC_DIR = path.join(__dirname, "..", "static");
const STATIC_FILES = {
  "/admin/static/htmx.js": path.join(STATIC_DIR, "htmx.js"),
  "/admin/static/json-enc.js": path.join(STATIC_DIR, "json-enc.js"),
};

/**
 * @param {Object<string, (url: URL) => {status?: number, body: string}>} routes
 * @returns {Promise<{url: string, close: () => Promise<void>}>}
 */
function startServer(routes) {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://localhost");
    if (url.pathname === "/") {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(fs.readFileSync(ADMIN_HTML_PATH, "utf8"));
      return;
    }
    if (STATIC_FILES[url.pathname]) {
      res.writeHead(200, { "Content-Type": "application/javascript; charset=utf-8" });
      res.end(fs.readFileSync(STATIC_FILES[url.pathname], "utf8"));
      return;
    }
    const handler = routes[url.pathname];
    if (!handler) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end(`no route stubbed for ${url.pathname}`);
      return;
    }
    const { status = 200, body } = handler(url);
    res.writeHead(status, { "Content-Type": "text/html; charset=utf-8" });
    res.end(body);
  });

  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      resolve({
        url: `http://127.0.0.1:${port}/`,
        close: () => new Promise((r) => server.close(r)),
      });
    });
  });
}

module.exports = { startServer };
