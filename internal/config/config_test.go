package config_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/config"
)

// MapLoader is a simple test helper implementing config.Loader.
type MapLoader map[string]string

func (m MapLoader) Get(key string) (string, error) {
	return m[key], nil
}

// validRequired returns a MapLoader with all required fields set.
// registration-key is optional and not included here.
func validRequired() MapLoader {
	return MapLoader{
		"account-name":   "my-account",
		"computer-title": "my-box",
		"url":            "https://landscape.example.com",
	}
}

func TestLoad_AllRequired_Valid(t *testing.T) {
	m := validRequired()
	m["registration-key"] = "secret" // optional but provided here
	cfg, err := config.Load(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AccountName != "my-account" {
		t.Errorf("AccountName = %q, want %q", cfg.AccountName, "my-account")
	}
	if cfg.RegistrationKey != "secret" {
		t.Errorf("RegistrationKey = %q, want %q", cfg.RegistrationKey, "secret")
	}
	if cfg.ComputerTitle != "my-box" {
		t.Errorf("ComputerTitle = %q, want %q", cfg.ComputerTitle, "my-box")
	}
	if cfg.URL != "https://landscape.example.com" {
		t.Errorf("URL = %q, want %q", cfg.URL, "https://landscape.example.com")
	}
}

func TestLoad_MissingSingleRequired(t *testing.T) {
	m := validRequired()
	delete(m, "account-name")
	_, err := config.Load(m)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "account-name") {
		t.Errorf("error %q does not mention account-name", err.Error())
	}
}

func TestLoad_MissingAllRequired(t *testing.T) {
	_, err := config.Load(MapLoader{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// registration-key is optional, so only these three must appear.
	for _, key := range []string{"account-name", "computer-title", "url"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %q", err.Error(), key)
		}
	}
	// registration-key must NOT appear in the missing-required error.
	if strings.Contains(err.Error(), "registration-key") {
		t.Errorf("error %q wrongly mentions registration-key as required", err.Error())
	}
}

func TestLoad_InvalidURL_NoScheme(t *testing.T) {
	m := validRequired()
	m["url"] = "landscape.example.com"
	_, err := config.Load(m)
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error %q should mention url", err.Error())
	}
}

func TestLoad_DurationField_Valid(t *testing.T) {
	m := validRequired()
	m["exchange-interval"] = "1800"
	cfg, err := config.Load(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExchangeInterval != 30*time.Minute {
		t.Errorf("ExchangeInterval = %v, want 30m", cfg.ExchangeInterval)
	}
}

func TestLoad_DurationField_Invalid(t *testing.T) {
	m := validRequired()
	m["exchange-interval"] = "not-a-number"
	_, err := config.Load(m)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "exchange-interval") {
		t.Errorf("error %q should mention exchange-interval", err.Error())
	}
}

func TestLoad_OptionalFieldsAbsent_Defaults(t *testing.T) {
	cfg, err := config.Load(validRequired())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExchangeInterval != 15*time.Minute {
		t.Errorf("ExchangeInterval = %v, want 15m", cfg.ExchangeInterval)
	}
	if cfg.UrgentExchangeInterval != 1*time.Minute {
		t.Errorf("UrgentExchangeInterval = %v, want 1m", cfg.UrgentExchangeInterval)
	}
	if cfg.PingInterval != 30*time.Second {
		t.Errorf("PingInterval = %v, want 30s", cfg.PingInterval)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.SSLPublicKey != "" {
		t.Errorf("SSLPublicKey = %q, want empty", cfg.SSLPublicKey)
	}
}

func TestLoad_OptionalFieldsPresent_Override(t *testing.T) {
	m := validRequired()
	m["exchange-interval"] = "300"
	m["urgent-exchange-interval"] = "10"
	m["ping-interval"] = "60"
	m["ssl-public-key"] = "/etc/ssl/ca.crt"
	m["http-proxy"] = "http://proxy.example.com"
	m["https-proxy"] = "https://proxy.example.com"
	m["access-group"] = "mygroup"
	m["tags"] = "tag1,tag2"
	m["log-level"] = "debug"

	cfg, err := config.Load(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExchangeInterval != 5*time.Minute {
		t.Errorf("ExchangeInterval = %v, want 5m", cfg.ExchangeInterval)
	}
	if cfg.UrgentExchangeInterval != 10*time.Second {
		t.Errorf("UrgentExchangeInterval = %v, want 10s", cfg.UrgentExchangeInterval)
	}
	if cfg.PingInterval != 1*time.Minute {
		t.Errorf("PingInterval = %v, want 1m", cfg.PingInterval)
	}
	if cfg.SSLPublicKey != "/etc/ssl/ca.crt" {
		t.Errorf("SSLPublicKey = %q, want /etc/ssl/ca.crt", cfg.SSLPublicKey)
	}
	if cfg.HTTPProxy != "http://proxy.example.com" {
		t.Errorf("HTTPProxy = %q, want http://proxy.example.com", cfg.HTTPProxy)
	}
	if cfg.HTTPSProxy != "https://proxy.example.com" {
		t.Errorf("HTTPSProxy = %q, want https://proxy.example.com", cfg.HTTPSProxy)
	}
	if cfg.AccessGroup != "mygroup" {
		t.Errorf("AccessGroup = %q, want mygroup", cfg.AccessGroup)
	}
	if cfg.Tags != "tag1,tag2" {
		t.Errorf("Tags = %q, want tag1,tag2", cfg.Tags)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestDefaults(t *testing.T) {
	d := config.Defaults()
	if d.ExchangeInterval != 15*time.Minute {
		t.Errorf("ExchangeInterval = %v, want 15m", d.ExchangeInterval)
	}
	if d.UrgentExchangeInterval != 1*time.Minute {
		t.Errorf("UrgentExchangeInterval = %v, want 1m", d.UrgentExchangeInterval)
	}
	if d.PingInterval != 30*time.Second {
		t.Errorf("PingInterval = %v, want 30s", d.PingInterval)
	}
	if d.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", d.LogLevel)
	}
	if d.AccountName != "" {
		t.Errorf("AccountName = %q, want empty", d.AccountName)
	}
	if d.RegistrationKey != "" {
		t.Errorf("RegistrationKey = %q, want empty", d.RegistrationKey)
	}
	if d.ComputerTitle != "" {
		t.Errorf("ComputerTitle = %q, want empty", d.ComputerTitle)
	}
	if d.URL != "" {
		t.Errorf("URL = %q, want empty", d.URL)
	}
}

// ErrorLoader returns an error for the specified key.
type ErrorLoader struct {
	errKey string
	base   MapLoader
}

func (e ErrorLoader) Get(key string) (string, error) {
	if key == e.errKey {
		return "", fmt.Errorf("simulated error for key %q", key)
	}
	return e.base.Get(key)
}

func TestLoad_LoaderError(t *testing.T) {
	l := ErrorLoader{
		errKey: "account-name",
		base: MapLoader{
			"registration-key": "key",
			"computer-title":   "title",
			"url":              "https://landscape.example.com",
		},
	}
	_, err := config.Load(l)
	if err == nil {
		t.Fatal("expected error from Loader.Get, got nil")
	}
	if !strings.Contains(err.Error(), "account-name") {
		t.Errorf("error should mention key name, got: %v", err)
	}
}

func TestLoad_HTTP_URL_Valid(t *testing.T) {
	m := validRequired()
	m["url"] = "http://landscape.example.com"
	cfg, err := config.Load(m)
	if err != nil {
		t.Fatalf("unexpected error for http URL: %v", err)
	}
	if cfg.URL != "http://landscape.example.com" {
		t.Errorf("URL = %q, want %q", cfg.URL, "http://landscape.example.com")
	}
}

// TestLoad_RegistrationKey_Optional verifies that omitting registration-key is valid.
func TestLoad_RegistrationKey_Optional(t *testing.T) {
	cfg, err := config.Load(validRequired())
	if err != nil {
		t.Fatalf("unexpected error when registration-key is absent: %v", err)
	}
	if cfg.RegistrationKey != "" {
		t.Errorf("RegistrationKey = %q, want empty", cfg.RegistrationKey)
	}
}

// TestValidateForHook_FreshInstall verifies that no error is returned when no config is set.
func TestValidateForHook_FreshInstall(t *testing.T) {
	if err := config.ValidateForHook(MapLoader{}); err != nil {
		t.Fatalf("expected nil for unconfigured snap, got: %v", err)
	}
}

// TestValidateForHook_PartialConfig verifies that partial configuration is tolerated.
func TestValidateForHook_PartialConfig(t *testing.T) {
	// Only url set — wizard in progress.
	if err := config.ValidateForHook(MapLoader{"url": "https://landscape.example.com"}); err != nil {
		t.Fatalf("expected nil for partial config, got: %v", err)
	}
	// Two of three required keys — still in progress.
	if err := config.ValidateForHook(MapLoader{
		"url":          "https://landscape.example.com",
		"account-name": "myaccount",
	}); err != nil {
		t.Fatalf("expected nil for partial config (2 keys), got: %v", err)
	}
}

// TestValidateForHook_FullyConfigured verifies that a complete valid config returns nil.
func TestValidateForHook_FullyConfigured(t *testing.T) {
	if err := config.ValidateForHook(validRequired()); err != nil {
		t.Fatalf("expected nil for valid full config, got: %v", err)
	}
}

// TestValidateForHook_InvalidConfig verifies that a fully-set but invalid config returns an error.
func TestValidateForHook_InvalidConfig(t *testing.T) {
	m := validRequired()
	m["url"] = "not-a-url" // missing scheme
	if err := config.ValidateForHook(m); err == nil {
		t.Fatal("expected error for invalid url, got nil")
	}
}
