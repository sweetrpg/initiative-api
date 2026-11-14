package main

import (
	"strings"
	"testing"
)

func env(overrides map[string]string) func(string) string {
	m := map[string]string{
		"AUTH_API_URL": "http://auth", "RECONCILER_CLIENT_ID": "id", "RECONCILER_CLIENT_SECRET": "secret",
		"RECONCILER_TOKEN_URL": "https://tenant.example/oauth/token", "RECONCILER_AUDIENCE": "aud", "REDIS_HOST": "cache",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func TestLoadSettingsAcceptsACompleteEnvironment(t *testing.T) {
	if _, err := loadSettings(env(nil)); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadSettingsNamesEachMissingVariable(t *testing.T) {
	for _, name := range []string{"AUTH_API_URL", "RECONCILER_CLIENT_ID", "RECONCILER_CLIENT_SECRET", "RECONCILER_TOKEN_URL", "RECONCILER_AUDIENCE", "REDIS_HOST"} {
		_, err := loadSettings(env(map[string]string{name: ""}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s unset: err = %v, want it named", name, err)
		}
	}
}

func TestLoadSettingsRejectsAnInsecureTokenURLWithoutEchoingTheSecret(t *testing.T) {
	for _, u := range []string{"http://tenant.example/oauth/token", "ftp://x/y", "https://", "https://user:pw@tenant.example/oauth/token", "not a url"} {
		_, err := loadSettings(env(map[string]string{"RECONCILER_TOKEN_URL": u}))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("token url %q: err = %v, want a rejection", u, err)
		}
	}
}
