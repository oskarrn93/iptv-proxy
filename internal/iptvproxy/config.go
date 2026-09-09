package iptvproxy

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Config is the top-level config.yaml shape: one entry per M3U8 feed, all
// served from a single shared listen address. A request's own
// username/password (not this process's listen port) is what picks which
// feed's upstream serves it — see Proxy.authenticate.
type Config struct {
	ListenAddr string `yaml:"listen_addr" validate:"required"`
	// unique=Username names the Go field promoted from the embedded
	// Credentials, which is what tells two feeds apart on the shared port.
	Feeds []FeedConfig `yaml:"feeds" validate:"min=1,unique=Username,dive"`
	// ClientBuffer configures the live-TS smoothing delay applied across
	// every feed (see ClientBufferConfig) — a proxy-wide setting rather than
	// a per-feed one, since it's about the client's playback experience, not
	// any one upstream's behavior.
	ClientBuffer ClientBufferConfig `yaml:"client_buffer"`
}

// defaultClientBufferDuration is how long a live TS stream is held back from
// the client when client_buffer.enabled is true and no duration is
// configured — long enough to absorb a brief upstream stall or reconnect
// without the client seeing one, short enough not to noticeably delay
// channel changes.
const defaultClientBufferDuration = 5 * time.Second

// ClientBufferConfig controls the delay buffer that smooths live TS
// streaming (see feedService.streamLive / delayedReader): every byte read
// from upstream is held for Duration before being written to the client,
// so a transient upstream hiccup or reconnect that resolves within that
// window never reaches the client as a stall. Enabled is a pointer so
// omitting client_buffer entirely from config.yaml is distinguishable from
// explicitly writing "enabled: false" — the former defaults to on.
type ClientBufferConfig struct {
	Enabled  *bool         `yaml:"enabled"`
	Duration time.Duration `yaml:"duration"`
}

// IsEnabled reports whether the client buffer should be applied, treating an
// unset Enabled (config omitted client_buffer, or omitted just its enabled
// key) as on by default.
func (c ClientBufferConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

type FeedConfig struct {
	Name        string `yaml:"name" validate:"required"`
	UpstreamURL string `yaml:"upstream_url" validate:"required,http_url"`
	// Credentials is embedded inline, so a feed still spells its own pair as
	// plain `username:`/`password:` keys — it is one value only so that the
	// two can't be passed around, matched, or rewritten separately.
	Credentials `yaml:",inline"`
	Timeout     time.Duration `yaml:"timeout"`
	// ExcludeGroupPrefixes drops any playlist entry whose group-title starts
	// with one of these prefixes (case-insensitive), e.g. ["movies:", "series:"]
	// to strip VOD out of an Xtream m3u_plus feed and keep only live channels.
	// Left empty, every entry is kept.
	ExcludeGroupPrefixes []string `yaml:"exclude_group_prefixes"`
}

// validate reports failures in terms of config.yaml's own keys rather than Go
// field names: registering the yaml tag as the field name means a new field
// needs no second registration anywhere to get a message an operator can act
// on.
var validate = func() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name, _, _ := strings.Cut(fld.Tag.Get("yaml"), ",")
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}()

// feedIndexPattern extracts the slice index from a validator namespace like
// "Config.feeds[2].upstream_url".
var feedIndexPattern = regexp.MustCompile(`feeds\[(\d+)\]`)

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validate.Struct(&cfg); err != nil {
		return nil, describeValidationError(&cfg, err)
	}

	for i := range cfg.Feeds {
		if cfg.Feeds[i].Timeout <= 0 {
			cfg.Feeds[i].Timeout = 15 * time.Second
		}
	}

	if cfg.ClientBuffer.Duration <= 0 {
		cfg.ClientBuffer.Duration = defaultClientBufferDuration
	}

	return &cfg, nil
}

// describeValidationError turns the first validator failure into a message
// phrased in terms of config.yaml's own keys, e.g. `feed "foo": listen_addr
// is required` rather than validator's generic `Key: 'Config.Feeds[0]...'`.
func describeValidationError(cfg *Config, err error) error {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		return fmt.Errorf("invalid config: %w", err)
	}
	fe := verrs[0]

	if m := feedIndexPattern.FindStringSubmatch(fe.Namespace()); m != nil {
		idx, _ := strconv.Atoi(m[1])
		feedRef := fmt.Sprintf("feed %d", idx)
		if idx < len(cfg.Feeds) && cfg.Feeds[idx].Name != "" {
			feedRef = fmt.Sprintf("feed %q", cfg.Feeds[idx].Name)
		}
		return fmt.Errorf("%s: %s", feedRef, describeFieldError(fe))
	}

	return errors.New(describeFieldError(fe))
}

// describeFieldError phrases one validator failure against the config.yaml key
// it concerns. The default names the key and the rule it broke rather than
// asserting a specific cause, so a field added later can't inherit a message
// about some unrelated rule.
func describeFieldError(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", fe.Field())
	case "http_url":
		return fmt.Sprintf("%s must be an absolute http:// or https:// URL", fe.Field())
	case "min":
		// The only "min" rule is feeds having at least one entry.
		return "config has no feeds defined"
	case "unique":
		// fe.Param() is the tag's literal argument (a Go field name), not a
		// yaml key, so it can't be reported directly.
		return "duplicate username across feeds"
	default:
		return fmt.Sprintf("%s is invalid (failed %q)", fe.Field(), fe.Tag())
	}
}
