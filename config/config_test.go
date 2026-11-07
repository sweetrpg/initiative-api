package config

import (
	"strings"
	"testing"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BindAddress != ":8000" || c.LogLevel != "WARN" || c.Env != "dev" || len(c.AllowedOrigins) != 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad bind address", map[string]string{"BIND_ADDRESS": "8000"}, "BIND_ADDRESS"},
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"relative otlp endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "tempo:4318"}, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{"wildcard origin", map[string]string{"ALLOWED_ORIGINS": "https://a.example,*"}, "wildcard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(lookupFrom(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{"BIND_ADDRESS": "x", "LOG_LEVEL": "loud"}))
	if err == nil || !strings.Contains(err.Error(), "BIND_ADDRESS") || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestLoadValid(t *testing.T) {
	c, err := Load(lookupFrom(map[string]string{
		"BIND_ADDRESS":                "0.0.0.0:8000",
		"LOG_LEVEL":                   "debug",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4318/",
		"ALLOWED_ORIGINS":             "https://a.example,https://b.example",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.LogLevel != "DEBUG" || len(c.AllowedOrigins) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
}
