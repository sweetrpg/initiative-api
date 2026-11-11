// Package config loads and validates the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/sweetrpg/initiative-api/constants"
)

// Config is the validated runtime configuration.
type Config struct {
	BindAddress string
	Env         string
	Version     string
	LogLevel    string
	// OTLPEndpoint is the trace collector base URL; empty leaves the exporter default.
	OTLPEndpoint string
	// AllowedOrigins is the CORS allowlist; empty disables CORS (fail closed).
	AllowedOrigins []string
	// AuthAPIURL is auth-api's base URL. Bearer tokens are verified there via /authz/check
	// (signature, expiry, iss, aud, roles); this service holds no signing keys or credentials.
	AuthAPIURL string
	// UsersAPIURL is users-api's base URL, used to resolve the verified caller to users._id.
	UsersAPIURL string
	// EntitlementsAPIURL is entitlements-api's base URL for gating checks.
	EntitlementsAPIURL string
}

var logLevels = map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}

// Load reads the environment through lookup (os.LookupEnv in production) and returns every
// validation problem at once, so a misconfigured deploy reports all of them in one start attempt.
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}

	c := Config{
		BindAddress:        get("BIND_ADDRESS", ":8000"),
		Env:                get("ENV", "dev"),
		Version:            get("VERSION", "v0.0.0"),
		LogLevel:           strings.ToUpper(get(constants.LOG_LEVEL, "WARN")),
		OTLPEndpoint:       get(constants.OTEL_EXPORTER_OTLP_ENDPOINT, ""),
		AuthAPIURL:         strings.TrimRight(get(constants.AUTH_API_URL, ""), "/"),
		UsersAPIURL:        strings.TrimRight(get(constants.USERS_API_URL, ""), "/"),
		EntitlementsAPIURL: strings.TrimRight(get(constants.ENTITLEMENTS_API_URL, ""), "/"),
	}
	if origins := get(constants.ALLOWED_ORIGINS, ""); origins != "" {
		c.AllowedOrigins = strings.Split(origins, ",")
	}

	var errs []error
	if _, _, err := net.SplitHostPort(c.BindAddress); err != nil {
		errs = append(errs, fmt.Errorf("BIND_ADDRESS %q: %w", c.BindAddress, err))
	}
	if !logLevels[c.LogLevel] {
		errs = append(errs, fmt.Errorf("%s %q: must be one of DEBUG, INFO, WARN, ERROR", constants.LOG_LEVEL, c.LogLevel))
	}
	errs = append(errs, checkURL(constants.OTEL_EXPORTER_OTLP_ENDPOINT, c.OTLPEndpoint, false)...)
	errs = append(errs, checkURL(constants.AUTH_API_URL, c.AuthAPIURL, true)...)
	errs = append(errs, checkURL(constants.USERS_API_URL, c.UsersAPIURL, true)...)
	errs = append(errs, checkURL(constants.ENTITLEMENTS_API_URL, c.EntitlementsAPIURL, true)...)
	for _, o := range c.AllowedOrigins {
		if strings.TrimSpace(o) == "*" {
			errs = append(errs, fmt.Errorf("%s: wildcard origin is not allowed", constants.ALLOWED_ORIGINS))
		}
	}

	return c, errors.Join(errs...)
}

// FromEnv is Load against the process environment.
func FromEnv() (Config, error) { return Load(os.LookupEnv) }

// checkURL validates an absolute http(s) URL; an empty value is an error only when required.
func checkURL(key, value string, required bool) []error {
	if value == "" {
		if required {
			return []error{fmt.Errorf("%s: required", key)}
		}
		return nil
	}
	if u, err := url.Parse(value); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return []error{fmt.Errorf("%s %q: must be an absolute http(s) URL", key, value)}
	}
	return nil
}
