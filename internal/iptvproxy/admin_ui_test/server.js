// server.js is a minimal stand-in for the real iptv-proxy admin server
// (admin.go's handleAdminPage/handleAdminHTMX/handleAdminJSONEnc/
// handleAdminFeeds/handleAdminChannels/handleListShares), used only so
// Playwright can drive the REAL admin.html (../admin.html — the file
// admin.go go:embeds and serves byte-for-byte) over real HTTP in a real
// browser, without needing a running iptv-proxy instance or real IPTV
// feeds. /admin/static/* is served from ../static/ — the exact files admin.go
// embeds — so there's no separate copy for these tests to fall out of sync
// with. Each test supplies its own
// `routes` (see admin.test.js) to control what /admin/feeds,
// /admin/channels, and /admin/shares return.
"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");

const ADMIN_HTML_PATH = path.join(__dirname, "..", "admin.html");
const VIEWS_HTML_PATH = path.join(__dirname, "..", "admin_views.html");
const STATIC_DIR = path.join(__dirname, "..", "static");
const STATIC_PREFIX = "/admin/static/";
const STATIC_TYPES = { ".js": "application/javascript", ".css": "text/css" };

/**
 * @param {Object<string, (url: URL, req: http.IncomingMessage) => {status?: number, body?: string}>} routes
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
    if (url.pathname === "/views") {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(fs.readFileSync(VIEWS_HTML_PATH, "utf8"));
      return;
    }
    if (url.pathname.startsWith(STATIC_PREFIX)) {
      const file = path.basename(url.pathname);
      res.writeHead(200, { "Content-Type": `${STATIC_TYPES[path.extname(file)]}; charset=utf-8` });
      res.end(fs.readFileSync(path.join(STATIC_DIR, file), "utf8"));
      return;
    }
    const handler = routes[url.pathname];
    if (!handler) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end(`no route stubbed for ${url.pathname}`);
      return;
    }
    const { status = 200, body } = handler(url, req);
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
