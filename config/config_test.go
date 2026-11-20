package config

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func withAuth(m map[string]string) map[string]string {
	out := map[string]string{"AUTH_API_URL": "http://auth-api:8000", "USERS_API_URL": "http://users-api:8000", "ENTITLEMENTS_API_URL": "http://entitlements-api:8000", "REDIS_HOST": "cache"}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookupFrom(withAuth(nil)))
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
		{"ticket ttl above the row ttl", map[string]string{"STREAM_TICKET_TTL": "5m"}, "STREAM_TICKET_TTL"},
		{"heartbeat too slow to bound revocation", map[string]string{"STREAM_HEARTBEAT": "10m"}, "STREAM_HEARTBEAT"},
		{"wildcard stream origin", map[string]string{"STREAM_ALLOWED_ORIGINS": "*"}, "wildcard"},
		{"double star stream origin", map[string]string{"STREAM_ALLOWED_ORIGINS": "**"}, "match any host"},
		{"star question stream origin", map[string]string{"STREAM_ALLOWED_ORIGINS": "*?*"}, "match any host"},
		{"malformed stream origin pattern", map[string]string{"STREAM_ALLOWED_ORIGINS": "app[.example.test"}, "not a valid host pattern"},
		{"request timeout too long", map[string]string{"REQUEST_TIMEOUT": "10m"}, "REQUEST_TIMEOUT"},
		{"credential outage limit too long", map[string]string{"CREDENTIAL_EVENTS_MAX_DISCONNECT": "48h"}, "CREDENTIAL_EVENTS_MAX_DISCONNECT"},
		{"lease shorter than two heartbeats", map[string]string{"STREAM_LEASE_TTL": "30s", "STREAM_HEARTBEAT": "20s"}, "at least twice"},
		{"concurrent cap of zero", map[string]string{"STREAM_CONCURRENT_MAX": "0"}, "STREAM_CONCURRENT_MAX"},
		{"credential cap too high", map[string]string{"STREAM_CREDENTIAL_MAX": "500"}, "STREAM_CREDENTIAL_MAX"},
		{"pod cap not a number", map[string]string{"STREAM_MAX_PER_POD": "many"}, "STREAM_MAX_PER_POD"},
		{"grace too long", map[string]string{"STREAM_GRACE": "24h"}, "STREAM_GRACE"},
		{"bad bind address", map[string]string{"BIND_ADDRESS": "8000"}, "BIND_ADDRESS"},
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"relative otlp endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "tempo:4318"}, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{"missing auth-api url", map[string]string{"AUTH_API_URL": " "}, "AUTH_API_URL: required"},
		{"missing entitlements-api url", map[string]string{"ENTITLEMENTS_API_URL": " "}, "ENTITLEMENTS_API_URL: required"},
		{"non-http users-api url", map[string]string{"USERS_API_URL": "ftp://users"}, "USERS_API_URL"},
		{"unparsable max age", map[string]string{"OWNER_ENTITLEMENTS_MAX_AGE": "week"}, "OWNER_ENTITLEMENTS_MAX_AGE"},
		{"zero session members max", map[string]string{"SESSION_MEMBERS_MAX": "0"}, "SESSION_MEMBERS_MAX"},
		{"non-numeric session members max", map[string]string{"SESSION_MEMBERS_MAX": "many"}, "SESSION_MEMBERS_MAX"},
		{"zero max age", map[string]string{"OWNER_ENTITLEMENTS_MAX_AGE": "0s"}, "OWNER_ENTITLEMENTS_MAX_AGE"},
		{"wildcard origin", map[string]string{"ALLOWED_ORIGINS": "https://a.example,*"}, "wildcard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(lookupFrom(withAuth(tt.env)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(lookupFrom(withAuth(map[string]string{"BIND_ADDRESS": "x", "LOG_LEVEL": "loud"})))
	if err == nil || !strings.Contains(err.Error(), "BIND_ADDRESS") || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestLoadValid(t *testing.T) {
	c, err := Load(lookupFrom(withAuth(map[string]string{
		"BIND_ADDRESS":                "0.0.0.0:8000",
		"LOG_LEVEL":                   "debug",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4318/",
		"ALLOWED_ORIGINS":             "https://a.example,https://b.example",
	})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.LogLevel != "DEBUG" || len(c.AllowedOrigins) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestLoadRequiresRedisHost(t *testing.T) {
	env := withAuth(nil)
	delete(env, "REDIS_HOST")

	_, err := Load(lookupFrom(env))

	if err == nil || !strings.Contains(err.Error(), "REDIS_HOST") {
		t.Fatalf("err = %v, want REDIS_HOST required", err)
	}
}

func TestLoadOwnerEntitlementsMaxAge(t *testing.T) {
	c, err := Load(lookupFrom(withAuth(nil)))
	if err != nil || c.OwnerEntitlementsMaxAge != 7*24*time.Hour {
		t.Fatalf("default = %v, %v; want 168h", c.OwnerEntitlementsMaxAge, err)
	}
	c, err = Load(lookupFrom(withAuth(map[string]string{"OWNER_ENTITLEMENTS_MAX_AGE": "36h"})))
	if err != nil || c.OwnerEntitlementsMaxAge != 36*time.Hour {
		t.Fatalf("set = %v, %v; want 36h", c.OwnerEntitlementsMaxAge, err)
	}
}

func TestLoadSessionMembersMax(t *testing.T) {
	c, err := Load(lookupFrom(withAuth(nil)))
	if err != nil || c.SessionMembersMax != 20 {
		t.Fatalf("default = %d, %v; want 20", c.SessionMembersMax, err)
	}
	c, err = Load(lookupFrom(withAuth(map[string]string{"SESSION_MEMBERS_MAX": "8"})))
	if err != nil || c.SessionMembersMax != 8 {
		t.Fatalf("set = %d, %v; want 8", c.SessionMembersMax, err)
	}
}
