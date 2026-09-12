// @ts-check
const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: __dirname,
  fullyParallel: true,
  reporter: "list",
  use: {
    // No real internet access needed or wanted — vendor/ + admin.test.js's
    // stubCDN() serve the htmx/json-enc scripts admin.html references.
  },
});
