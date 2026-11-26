// Visual regression tests for the admin page. The baselines in
// docs/screenshots are also the images the README shows, so `npm run
// screenshots` updates both at once. Baselines are rendered on macOS and font
// rendering differs elsewhere, so CI skips anything tagged @visual.
"use strict";

const { test, expect } = require("@playwright/test");
const { startServer } = require("./server");

const CHANNELS = [
  "Sports 1 HD",
  "Sports 2 HD",
  "Sports Extra",
  "News 24",
  "Documentary Channel",
  "Movies Classic",
];

// Mirrors the templates in admin_fragments.html.
const options = (names) => names.map((n, i) => `<option value="http://iptv.example.com/live/u/p/${i}.ts">${n}</option>`).join("");
const shareRow = (token, channel, feed, expires) =>
  `<tr id="share-${token}"><td>${channel}</td><td>${feed}</td><td>live</td><td>${expires}</td>` +
  `<td><a href="#">watch</a></td><td><button class="small">revoke</button></td></tr>`;
const created =
  "<p>Share created for <strong>Sports 1 HD</strong>, expires <strong>2026-11-09 18:30:00 UTC</strong>:</p>" +
  '<p><a href="#">https://iptv.example.com/share/7f3a9c1e5b2d4860a1c3e5f7092b4d6e</a></p>';

function routes() {
  let submitted = false;
  return {
    "/admin/feeds": () => ({ body: '<option value="home">home</option><option value="travel">travel</option>' }),
    "/admin/channels": () => ({ body: options(CHANNELS) }),
    "/admin/shares": (_url, req) => {
      if (req.method === "POST") {
        submitted = true;
        return { body: created };
      }
      const rows = shareRow("b", "News 24", "travel", "21:15:00 UTC");
      return { body: submitted ? shareRow("a", "Sports 1 HD", "home", "18:30:00 UTC") + rows : rows };
    },
  };
}

test.use({ viewport: { width: 800, height: 720 } });

async function open(page) {
  const server = await startServer(routes());
  await page.goto(server.url);
  await expect(page.locator("#channel-search")).toHaveAttribute("placeholder", "Search channel...");
  return server;
}

test("admin page after creating a share @visual", async ({ page }) => {
  const server = await open(page);
  try {
    await page.locator("#channel-search").focus();
    await page.keyboard.press("Enter");
    await page.getByRole("button", { name: "Create share link" }).click();
    await expect(page.locator("#share-a")).toBeVisible();
    await page.locator("h1").click();
    await expect(page).toHaveScreenshot("admin.png", { fullPage: true });
  } finally {
    await server.close();
  }
});

test("channel search dropdown @visual", async ({ page }) => {
  const server = await open(page);
  try {
    await page.locator("#channel-search").fill("sports");
    await expect(page.locator("#channel-list .combo-option")).toHaveCount(3);
    await expect(page).toHaveScreenshot("admin-channel-search.png");
  } finally {
    await server.close();
  }
});

const viewRow = (name, feed, user, summary) =>
  `<tr id="view-${name}"><td>${name}</td><td>${feed}</td><td>${user}</td><td>${summary}</td>` +
  `<td><button class="small">edit</button> <button class="small">delete</button></td></tr>`;

const editor = `<form id="view-form">
  <h2>Edit kids</h2>
  <input type="hidden" name="original" value="kids" />
  <label for="view-name">Name</label>
  <input id="view-name" name="name" value="kids" readonly />
  <label for="view-feed">Feed</label>
  <select id="view-feed" name="feed"><option value="home" selected>home</option></select>
  <label for="view-username">Username</label>
  <input id="view-username" name="username" value="kidsviewer" />
  <label for="view-password">Password</label>
  <input id="view-password" name="password" value="4f9c2a7e1b" />
  <label for="view-include">Include</label>
  <textarea id="view-include" name="include" rows="4">group: ^Kids</textarea>
  <label for="view-exclude">Exclude</label>
  <textarea id="view-exclude" name="exclude" rows="3">name: (?i)adult</textarea>
  <button type="submit">Save view</button>
</form>`;

test("views admin page @visual", async ({ page }) => {
  const server = await startServer({
    "/admin/views/list": () => ({
      body:
        viewRow("kids", "home", "kidsviewer", "1 include, 1 exclude") +
        viewRow("sports", "travel", "sp0rt5", "2 includes, 3 pinned"),
    }),
    "/admin/views/editor": () => ({ body: editor }),
  });
  try {
    await page.goto(new URL("views", server.url).href);
    await expect(page.locator("#view-kids")).toBeVisible();
    await page.getByRole("button", { name: "New view" }).click();
    await expect(page.locator("#view-form")).toBeVisible();
    await expect(page).toHaveScreenshot("admin-views.png", { fullPage: true });
  } finally {
    await server.close();
  }
});
