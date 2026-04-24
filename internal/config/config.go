package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Config holds validated client configuration.
type Config struct {
	// Required
	AccountName     string
	RegistrationKey string
	ComputerTitle   string
	URL             string

	// Optional with defaults
	ExchangeInterval       time.Duration // default: 15 * time.Minute
	UrgentExchangeInterval time.Duration // default: 1 * time.Minute
	PingInterval           time.Duration // default: 30 * time.Second
	SSLPublicKey           string        // path to CA cert; default: ""
	HTTPProxy              string
	HTTPSProxy             string
	AccessGroup            string
	Tags                   string
	LogLevel               string // default: "info"
}

// Loader abstracts snapctl (or any key-value store) for testing.
type Loader interface {
	Get(key string) (string, error)
}

// Defaults returns a Config with all optional fields set to their defaults.
// Required fields are empty strings — useful for test setup.
func Defaults() *Config {
	return &Config{
		ExchangeInterval:       15 * time.Minute,
		UrgentExchangeInterval: 1 * time.Minute,
		PingInterval:           30 * time.Second,
		LogLevel:               "info",
	}
}

// Load reads and validates configuration from the given Loader.
// Returns an error if any required field is missing, listing all missing fields.
// Optional fields that are absent receive their default values.
func Load(l Loader) (*Config, error) {
	c := Defaults()

	// Required fields: key -> pointer to field
	type requiredField struct {
		key   string
		field *string
	}
	required := []requiredField{
		{"account-name", &c.AccountName},
		{"registration-key", &c.RegistrationKey},
		{"computer-title", &c.ComputerTitle},
		{"url", &c.URL},
	}

	var missing []string
	for _, rf := range required {
		v, err := l.Get(rf.key)
		if err != nil {
			return nil, fmt.Errorf("reading config key %q: %w", rf.key, err)
		}
		if v == "" {
			missing = append(missing, rf.key)
		} else {
			*rf.field = v
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}

	// Validate URL scheme
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return nil, fmt.Errorf("invalid url %q: must start with http:// or https://", c.URL)
	}

	// Duration fields: key -> pointer to field, default value
	type durationField struct {
		key          string
		field        *time.Duration
		defaultValue time.Duration
	}
	durations := []durationField{
		{"exchange-interval", &c.ExchangeInterval, 15 * time.Minute},
		{"urgent-exchange-interval", &c.UrgentExchangeInterval, 1 * time.Minute},
		{"ping-interval", &c.PingInterval, 30 * time.Second},
	}

	for _, df := range durations {
		v, err := l.Get(df.key)
		if err != nil {
			return nil, fmt.Errorf("reading config key %q: %w", df.key, err)
		}
		if v == "" {
			*df.field = df.defaultValue
		} else {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("invalid duration for %q: %w", df.key, err)
			}
			*df.field = d
		}
	}

	// Optional string fields
	type optionalField struct {
		key   string
		field *string
	}
	optional := []optionalField{
		{"ssl-public-key", &c.SSLPublicKey},
		{"http-proxy", &c.HTTPProxy},
		{"https-proxy", &c.HTTPSProxy},
		{"access-group", &c.AccessGroup},
		{"tags", &c.Tags},
		{"log-level", &c.LogLevel},
	}

	for _, of := range optional {
		v, err := l.Get(of.key)
		if err != nil {
			return nil, fmt.Errorf("reading config key %q: %w", of.key, err)
		}
		if v != "" {
			*of.field = v
		}
		// log-level default is already set by Defaults(); empty means keep default
	}

	return c, nil
}
