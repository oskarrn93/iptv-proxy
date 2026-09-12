// Regression tests for the admin page's feed/channel comboboxes
// (admin.html's <script> at the bottom, backed by admin.go's
// handleAdminFeeds/handleAdminChannels). These exist because the wiring
// between htmx's hx-get/hx-trigger attributes and the hand-rolled
// setupCombobox() JS is easy to silently break — e.g. pre-selecting a
// <select>'s value without dispatching a "change" event leaves htmx's
// hx-trigger="change" never firing, so a dependent dropdown quietly stays
// empty even though the feed appears selected.
//
// This drives the real admin.html (../admin.html, byte-for-byte what
// admin.go go:embeds) in a real Chromium via Playwright, served by
// server.js — which also serves the same vendored htmx/json-enc files
// (../static/) that admin.go embeds and admin.html's <script src> tags
// point at, so there's no CDN dependency and no separate copy to keep in
// sync. Every other behavior — DOM, XHR, event dispatch, htmx itself — is
// the real thing, not a reimplementation.
"use strict";

const { test, expect } = require("@playwright/test");
const { startServer } = require("./server");

const FEEDS_HTML =
  '<option value="feed-1">feed-1</option><option value="feed-2">feed-2</option>';

function channelsHTMLFor(feed) {
  return (
    `<option value="http://x/${feed}/1.ts">Channel One</option>` +
    `<option value="http://x/${feed}/2.ts">Channel Two</option>`
  );
}

function standardRoutes() {
  return {
    "/admin/feeds": () => ({ body: FEEDS_HTML }),
    "/admin/channels": (u) => ({
      body: channelsHTMLFor(u.searchParams.get("feed")),
    }),
    "/admin/shares": () => ({ body: "<tr><td>no shares</td></tr>" }),
  };
}

async function openAdminPage(page, routes) {
  const server = await startServer(routes);
  const requests = [];
  page.on("request", (req) => {
    const u = new URL(req.url());
    if (u.pathname.startsWith("/admin/")) requests.push(u);
  });
  const pageErrors = [];
  page.on("pageerror", (err) => pageErrors.push(err));
  await page.goto(server.url);
  return { server, requests, pageErrors };
}

test("feed combobox pre-selects the first feed on load", async ({ page }) => {
  const { server, pageErrors } = await openAdminPage(page, standardRoutes());
  try {
    await expect(page.locator("#feed-search")).toHaveValue("feed-1");
    await expect(page.locator("#feed")).toHaveValue("feed-1");
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("auto-selecting the first feed populates its channels (the bug this suite guards against)", async ({
  page,
}) => {
  const { server, pageErrors } = await openAdminPage(page, standardRoutes());
  try {
    // The channel combo-list (fetched manually, not swapped in by htmx —
    // see loadChannelsFor) must reflect the fetched options.
    await expect(page.locator("#channel-list .combo-option")).toHaveCount(2);
    await expect(page.locator("#channel-list .combo-option").first()).toHaveText(
      "Channel One",
    );
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("picking a different feed re-populates the channel dropdown for that feed", async ({
  page,
}) => {
  const { server, pageErrors } = await openAdminPage(page, standardRoutes());
  try {
    // Regression coverage for a real bug: focusing the search box used to
    // re-filter by its current text (the auto-selected "feed-1"), hiding
    // every other feed so there was nothing left to click. Opening the
    // dropdown must show every option regardless of what's already typed.
    await page.locator("#feed-search").click();
    await page.locator('#feed-list .combo-option[data-value="feed-2"]').click();

    await expect(page.locator("#feed")).toHaveValue("feed-2");
    await expect(page.locator("#channel-list .combo-option")).toHaveCount(2);
    const channelValues = await page
      .locator("#channel-list .combo-option")
      .evaluateAll((opts) => opts.map((o) => o.dataset.value));
    expect(channelValues).toEqual([
      "http://x/feed-2/1.ts",
      "http://x/feed-2/2.ts",
    ]);
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("focusing the feed search box shows every feed, not just ones matching the already-selected label", async ({
  page,
}) => {
  const { server, pageErrors } = await openAdminPage(page, standardRoutes());
  try {
    // feed-1 is auto-selected, so #feed-search already reads "feed-1".
    await expect(page.locator("#feed-search")).toHaveValue("feed-1");
    await page.locator("#feed-search").click();
    await expect(page.locator("#feed-list .combo-option:not([hidden])")).toHaveCount(2);
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("a channel list with tens of thousands of entries doesn't freeze the page", async ({
  page,
}) => {
  // Regression test for a real incident: some Xtream panels return tens of
  // thousands of "live" entries for one feed. setupCombobox used to build
  // one DOM node + mousedown listener per option eagerly on every rebuild,
  // which blocked the main thread for minutes on a list this size — long
  // enough that the page looked hung and no channel/feed was selectable.
  // Confirmed against the real feature: the backing <select> filled with
  // 30684 options in well under a second, but the old code took 195+
  // seconds to finish building the visible dropdown for it.
  const CHANNEL_COUNT = 30000;
  const routes = standardRoutes();
  routes["/admin/channels"] = () => ({
    body: Array.from(
      { length: CHANNEL_COUNT },
      (_, i) => `<option value="http://x/ch${i}.ts">Channel ${i}</option>`,
    ).join(""),
  });
  const { server, pageErrors } = await openAdminPage(page, routes);
  try {
    const start = Date.now();
    await expect(page.locator("#channel-list .combo-option")).not.toHaveCount(0);
    const elapsedMs = Date.now() - start;
    expect(elapsedMs).toBeLessThan(2000);

    await page.locator("#channel-search").click();

    // Only a bounded slice is ever in the DOM at once, not all 30000.
    const rowCount = await page.locator("#channel-list .combo-option").count();
    expect(rowCount).toBeLessThan(500);

    // Typing still finds a match anywhere in the full list, not just the
    // first slice that was rendered.
    await page.locator("#channel-search").fill("Channel 25000");
    await expect(page.locator('#channel-list .combo-option[data-value="http://x/ch25000.ts"]')).toBeVisible();

    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("no feeds configured: feed combo shows the empty state and never requests channels", async ({
  page,
}) => {
  const routes = standardRoutes();
  routes["/admin/feeds"] = () => ({ body: "" });
  const { server, requests, pageErrors } = await openAdminPage(page, routes);
  try {
    await expect(page.locator("#feed option")).toHaveCount(0);
    await expect(page.locator("#feed-list")).toContainText(
      "no feeds configured",
    );

    const channelRequests = requests.filter((u) =>
      u.pathname.includes("/admin/channels"),
    );
    expect(channelRequests).toEqual([]);
    await expect(page.locator("#channel-list .combo-option")).toHaveCount(1);
    await expect(page.locator("#channel-list")).toContainText(
      "no channels — pick a feed",
    );
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});

test("a feed with no channels shows the channel combo's empty state, not a stale list", async ({
  page,
}) => {
  const routes = standardRoutes();
  routes["/admin/channels"] = () => ({ body: "" });
  const { server, pageErrors } = await openAdminPage(page, routes);
  try {
    await expect(page.locator("#channel")).toHaveValue("");
    await expect(page.locator("#channel-list")).toContainText("no channels");
    expect(pageErrors).toEqual([]);
  } finally {
    await server.close();
  }
});
