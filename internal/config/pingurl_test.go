package config_test

import (
	"testing"

	"github.com/canonical/landscape-client-core/internal/config"
)

func TestGetPingURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		pingURL string
		want    string
	}{
		{"explicit ping URL wins", "https://landscape.example.com/message-system", "https://ping.example.com/p", "https://ping.example.com/p"},
		{"derived from https URL uses http", "https://landscape.example.com/message-system", "", "http://landscape.example.com/ping"},
		{"derived keeps port", "https://landscape.example.com:8443/message-system", "", "http://landscape.example.com:8443/ping"},
		{"empty URL", "", "", ""},
		{"URL without host", "not-a-url", "", ""},
		{"unparseable URL", "http://[::1", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.URL = tc.url
			cfg.PingURL = tc.pingURL
			if got := cfg.GetPingURL(); got != tc.want {
				t.Errorf("GetPingURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
