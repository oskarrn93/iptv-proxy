package iptvproxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestLoadConfig_Valid(t *testing.T) {
	path := writeConfig(t, `
feeds:
  - name: feed1
    listen_addr: ":8081"
    upstream_url: "http://example.com/get.php"
    username: user1
    password: pass1
    timeout: 30s
  - name: feed2
    listen_addr: ":8082"
    upstream_url: "http://example.com/get2.php"
    username: user2
    password: pass2
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Feeds, 2)

	assert.Equal(t, "feed1", cfg.Feeds[0].Name)
	assert.Equal(t, 30*time.Second, cfg.Feeds[0].Timeout)

	// Missing timeout defaults to 15s.
	assert.Equal(t, "feed2", cfg.Feeds[1].Name)
	assert.Equal(t, 15*time.Second, cfg.Feeds[1].Timeout)
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := writeConfig(t, "not: valid: yaml: [")
	_, err := LoadConfig(path)
	assert.Error(t, err)
}

func TestLoadConfig_NoFeeds(t *testing.T) {
	path := writeConfig(t, "feeds: []")
	_, err := LoadConfig(path)
	assert.ErrorContains(t, err, "no feeds")
}

func TestLoadConfig_MissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing name",
			yaml: `
feeds:
  - listen_addr: ":8081"
    upstream_url: "http://example.com"
    username: u
    password: p
`,
			wantErr: "name is required",
		},
		{
			name: "missing listen_addr",
			yaml: `
feeds:
  - name: feed1
    upstream_url: "http://example.com"
    username: u
    password: p
`,
			wantErr: "listen_addr is required",
		},
		{
			name: "missing upstream_url",
			yaml: `
feeds:
  - name: feed1
    listen_addr: ":8081"
    username: u
    password: p
`,
			wantErr: "upstream_url is required",
		},
		{
			name: "missing username",
			yaml: `
feeds:
  - name: feed1
    listen_addr: ":8081"
    upstream_url: "http://example.com"
    password: p
`,
			wantErr: "username is required",
		},
		{
			name: "missing password",
			yaml: `
feeds:
  - name: feed1
    listen_addr: ":8081"
    upstream_url: "http://example.com"
    username: u
`,
			wantErr: "password is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.yaml)
			_, err := LoadConfig(path)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadConfig_DuplicateListenAddr(t *testing.T) {
	path := writeConfig(t, `
feeds:
  - name: feed1
    listen_addr: ":8081"
    upstream_url: "http://example.com"
    username: u1
    password: p1
  - name: feed2
    listen_addr: ":8081"
    upstream_url: "http://example.com"
    username: u2
    password: p2
`)
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate listen_addr")
}
