// Package config loads and validates the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

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
	// RedisHost is the rate limiter's counter store; the limiter fails closed without it
	// (PADR-0036), so it is required. Port and password are read by the Redis pool itself.
	RedisHost string
	// OwnerEntitlementsMaxAge is how long a recorded owner entitlement value stays usable for
	// integration credentials, which cannot read it live. Past it the credential gets
	// limits_not_synced until the owner opens the app.
	OwnerEntitlementsMaxAge time.Duration
	// SessionMembersMax is the maximum of a session's member limit: an entitled members_per_session
	// above it has no effect (initiative-entitlement-gating, "Session member limit").
	SessionMembersMax int
	// CredentialEventsMaxDisconnect is how long NATS may be unreachable before integration
	// credentials fail closed (503); the rest of the API keeps serving.
	CredentialEventsMaxDisconnect time.Duration
	// StreamAllowedOrigins are the browser origins (host patterns) that may open an event stream; a
	// request with no Origin header (a native client) is always allowed.
	StreamAllowedOrigins []string
	// StreamTicketTTL is how long a stream ticket can be redeemed; StreamMaxAge ends every stream
	// after that long, so a revoked credential or a missed eviction cannot outlive it;
	// StreamHeartbeat is the interval of the server's heartbeat frames.
	StreamTicketTTL, StreamMaxAge, StreamHeartbeat time.Duration
}

// defaultOwnerEntitlementsMaxAge is seven days.
const defaultOwnerEntitlementsMaxAge = "168h"

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
		RedisHost:          get("REDIS_HOST", ""),
	}
	if origins := get(constants.ALLOWED_ORIGINS, ""); origins != "" {
		c.AllowedOrigins = strings.Split(origins, ",")
	}

	var errs []error
	maxMembers := get(constants.SESSION_MEMBERS_MAX, strconv.Itoa(constants.DefaultSessionMembersMax))
	if n, err := strconv.Atoi(maxMembers); err != nil || n < 1 {
		errs = append(errs, fmt.Errorf("%s %q: must be a positive integer", constants.SESSION_MEMBERS_MAX, maxMembers))
	} else {
		c.SessionMembersMax = n
	}
	maxAge := get(constants.OWNER_ENTITLEMENTS_MAX_AGE, defaultOwnerEntitlementsMaxAge)
	if d, err := time.ParseDuration(maxAge); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("%s %q: must be a positive duration such as 168h", constants.OWNER_ENTITLEMENTS_MAX_AGE, maxAge))
	} else {
		c.OwnerEntitlementsMaxAge = d
	}
	for _, d := range []struct {
		key      string
		dst      *time.Duration
		def      time.Duration
		min, max time.Duration
	}{
		{constants.STREAM_TICKET_TTL, &c.StreamTicketTTL, constants.DefaultStreamTicketTTL, time.Second, constants.MaxStreamTicketTTL},
		{constants.STREAM_MAX_AGE, &c.StreamMaxAge, constants.DefaultStreamMaxAge, time.Minute, 24 * time.Hour},
		// The heartbeat is also how often a stream re-checks membership and credentials, so it
		// bounds how long a revocation can go unseen.
		{constants.STREAM_HEARTBEAT, &c.StreamHeartbeat, constants.DefaultStreamHeartbeat, time.Second, time.Minute},
	} {
		v := get(d.key, d.def.String())
		if n, err := time.ParseDuration(v); err != nil || n < d.min || n > d.max {
			errs = append(errs, fmt.Errorf("%s %q: must be a duration between %s and %s", d.key, v, d.min, d.max))
		} else {
			*d.dst = n
		}
	}
	if origins := get(constants.STREAM_ALLOWED_ORIGINS, ""); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if o = strings.TrimSpace(o); o == "*" {
				errs = append(errs, fmt.Errorf("%s: wildcard origin is not allowed", constants.STREAM_ALLOWED_ORIGINS))
			} else if matchesArbitraryHost(o) {
				errs = append(errs, fmt.Errorf("%s %q: pattern would match any host", constants.STREAM_ALLOWED_ORIGINS, o))
			} else if _, err := path.Match(o, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s %q: not a valid host pattern", constants.STREAM_ALLOWED_ORIGINS, o))
			} else if o != "" {
				c.StreamAllowedOrigins = append(c.StreamAllowedOrigins, o)
			}
		}
	}
	maxDisc := get(constants.CREDENTIAL_EVENTS_MAX_DISCONNECT, "10m")
	if d, err := time.ParseDuration(maxDisc); err != nil || d < 30*time.Second || d > time.Hour {
		errs = append(errs, fmt.Errorf("%s %q: must be a duration between 30s and 1h", constants.CREDENTIAL_EVENTS_MAX_DISCONNECT, maxDisc))
	} else {
		c.CredentialEventsMaxDisconnect = d
	}
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
	if c.RedisHost == "" {
		errs = append(errs, errors.New("REDIS_HOST: required"))
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

// matchesArbitraryHost reports whether pattern matches hosts it does not name (such as "**" or
// "*?*"), which would defeat the ban on a wildcard origin.
func matchesArbitraryHost(pattern string) bool {
	for _, host := range []string{"a.b", "zz.example.test", "q"} {
		if ok, _ := path.Match(pattern, host); ok {
			return true
		}
	}
	return false
}
