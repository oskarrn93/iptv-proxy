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
listen_addr: ":58033"
feeds:
  - name: feed1
    upstream_url: "http://example.com/get.php"
    username: user1
    password: pass1
    timeout: 30s
  - name: feed2
    upstream_url: "http://example.com/get2.php"
    username: user2
    password: pass2
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, ":58033", cfg.ListenAddr)
	require.Len(t, cfg.Feeds, 2)

	assert.Equal(t, "feed1", cfg.Feeds[0].Name)
	assert.Equal(t, 30*time.Second, cfg.Feeds[0].Timeout)
	// Credentials is embedded inline, so the pair still comes from a feed's
	// own plain username:/password: keys rather than a nested mapping.
	assert.Equal(t, Credentials{Username: "user1", Password: "pass1"}, cfg.Feeds[0].Credentials)

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
	path := writeConfig(t, "listen_addr: \":58033\"\nfeeds: []")
	_, err := LoadConfig(path)
	assert.ErrorContains(t, err, "no feeds")
}

func TestLoadConfig_MissingListenAddr(t *testing.T) {
	path := writeConfig(t, `
feeds:
  - name: feed1
    upstream_url: "http://example.com"
    username: u
    password: p
`)
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listen_addr is required")
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
listen_addr: ":58033"
feeds:
  - upstream_url: "http://example.com"
    username: u
    password: p
`,
			wantErr: "name is required",
		},
		{
			name: "missing upstream_url",
			yaml: `
listen_addr: ":58033"
feeds:
  - name: feed1
    username: u
    password: p
`,
			wantErr: "upstream_url is required",
		},
		{
			name: "missing username",
			yaml: `
listen_addr: ":58033"
feeds:
  - name: feed1
    upstream_url: "http://example.com"
    password: p
`,
			wantErr: "username is required",
		},
		{
			name: "missing password",
			yaml: `
listen_addr: ":58033"
feeds:
  - name: feed1
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

func TestLoadConfig_DuplicateUsername(t *testing.T) {
	path := writeConfig(t, `
listen_addr: ":58033"
feeds:
  - name: feed1
    upstream_url: "http://example.com"
    username: dupuser
    password: p1
  - name: feed2
    upstream_url: "http://example.com"
    username: dupuser
    password: p2
`)
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate username")
}
