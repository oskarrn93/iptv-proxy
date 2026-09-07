package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level config.yaml shape: one entry per M3U8 feed,
// each served on its own listen address/port within this one process.
type Config struct {
	Feeds []FeedConfig `yaml:"feeds"`
}

type FeedConfig struct {
	Name        string        `yaml:"name"`
	ListenAddr  string        `yaml:"listen_addr"`
	UpstreamURL string        `yaml:"upstream_url"`
	Username    string        `yaml:"username"`
	Password    string        `yaml:"password"`
	Timeout     time.Duration `yaml:"timeout"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if len(cfg.Feeds) == 0 {
		return nil, errors.New("config has no feeds defined")
	}

	seenAddrs := make(map[string]string, len(cfg.Feeds))
	for i := range cfg.Feeds {
		f := &cfg.Feeds[i]
		if f.Name == "" {
			return nil, fmt.Errorf("feed %d: name is required", i)
		}
		if f.ListenAddr == "" {
			return nil, fmt.Errorf("feed %q: listen_addr is required", f.Name)
		}
		if f.UpstreamURL == "" {
			return nil, fmt.Errorf("feed %q: upstream_url is required", f.Name)
		}
		if f.Username == "" || f.Password == "" {
			return nil, fmt.Errorf("feed %q: username and password are required", f.Name)
		}
		if other, dup := seenAddrs[f.ListenAddr]; dup {
			return nil, fmt.Errorf("feed %q: listen_addr %q already used by feed %q", f.Name, f.ListenAddr, other)
		}
		seenAddrs[f.ListenAddr] = f.Name
		if f.Timeout <= 0 {
			f.Timeout = 15 * time.Second
		}
	}

	return &cfg, nil
}
