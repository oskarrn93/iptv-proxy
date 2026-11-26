.PHONY: screenshots

# Regenerates docs/screenshots (macOS only: baselines depend on font rendering).
screenshots:
	cd internal/iptvproxy/admin_ui_test && npm ci && npx playwright install chromium && npm run screenshots
