// @ts-check
const path = require("node:path");
const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: __dirname,
  fullyParallel: true,
  reporter: "list",
  // No platform suffix: these are also the images the README embeds.
  snapshotPathTemplate: path.join(__dirname, "../../../docs/screenshots/{arg}{ext}"),
  use: {
    // No real internet access needed or wanted — vendor/ + admin.test.js's
    // stubCDN() serve the htmx/json-enc scripts admin.html references.
  },
});
