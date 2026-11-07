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
		BindAddress:  get("BIND_ADDRESS", ":8000"),
		Env:          get("ENV", "dev"),
		Version:      get("VERSION", "v0.0.0"),
		LogLevel:     strings.ToUpper(get(constants.LOG_LEVEL, "WARN")),
		OTLPEndpoint: get(constants.OTEL_EXPORTER_OTLP_ENDPOINT, ""),
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
	if c.OTLPEndpoint != "" {
		if u, err := url.Parse(c.OTLPEndpoint); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s %q: must be an absolute URL", constants.OTEL_EXPORTER_OTLP_ENDPOINT, c.OTLPEndpoint))
		}
	}
	for _, o := range c.AllowedOrigins {
		if strings.TrimSpace(o) == "*" {
			errs = append(errs, fmt.Errorf("%s: wildcard origin is not allowed", constants.ALLOWED_ORIGINS))
		}
	}

	return c, errors.Join(errs...)
}

// FromEnv is Load against the process environment.
func FromEnv() (Config, error) { return Load(os.LookupEnv) }
