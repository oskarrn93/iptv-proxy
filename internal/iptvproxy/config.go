package iptvproxy

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Config is the top-level config.yaml shape: one entry per M3U8 feed,
// each served on its own listen address/port within this one process.
type Config struct {
	Feeds []FeedConfig `yaml:"feeds" validate:"min=1,unique=ListenAddr,dive"`
}

type FeedConfig struct {
	Name        string        `yaml:"name" validate:"required"`
	ListenAddr  string        `yaml:"listen_addr" validate:"required"`
	UpstreamURL string        `yaml:"upstream_url" validate:"required,http_url"`
	Username    string        `yaml:"username" validate:"required"`
	Password    string        `yaml:"password" validate:"required"`
	Timeout     time.Duration `yaml:"timeout"`
}

var validate = validator.New(validator.WithRequiredStructEnabled())

// feedYAMLKey maps a FeedConfig struct field name to its config.yaml key, so
// validation errors read in terms of the file the operator actually edited.
var feedYAMLKey = map[string]string{
	"Name":        "name",
	"ListenAddr":  "listen_addr",
	"UpstreamURL": "upstream_url",
	"Username":    "username",
	"Password":    "password",
}

// feedIndexPattern extracts the slice index from a validator namespace like
// "Config.Feeds[2].UpstreamURL".
var feedIndexPattern = regexp.MustCompile(`Feeds\[(\d+)\]`)

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
		key := feedYAMLKey[fe.Field()]

		switch fe.Tag() {
		case "http_url":
			return fmt.Errorf("%s: %s must be an absolute http:// or https:// URL", feedRef, key)
		default:
			return fmt.Errorf("%s: %s is required", feedRef, key)
		}
	}

	switch fe.Tag() {
	case "unique":
		return fmt.Errorf("duplicate %s across feeds", feedYAMLKey[fe.Param()])
	default: // "min": Feeds is empty
		return errors.New("config has no feeds defined")
	}
}
