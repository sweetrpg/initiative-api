// Package constants holds service-wide names and environment variable keys.
package constants

import "time"

// Environment variable names.
const (
	LOG_LEVEL                   = "LOG_LEVEL"
	OTEL_EXPORTER_OTLP_ENDPOINT = "OTEL_EXPORTER_OTLP_ENDPOINT"
	AUTH_API_URL                = "AUTH_API_URL"
	USERS_API_URL               = "USERS_API_URL"
	ENTITLEMENTS_API_URL        = "ENTITLEMENTS_API_URL"
	ALLOWED_ORIGINS             = "ALLOWED_ORIGINS"
	OWNER_ENTITLEMENTS_MAX_AGE  = "OWNER_ENTITLEMENTS_MAX_AGE"
	SESSION_MEMBERS_MAX         = "SESSION_MEMBERS_MAX"
	STREAM_ALLOWED_ORIGINS      = "STREAM_ALLOWED_ORIGINS"
	STREAM_TICKET_TTL           = "STREAM_TICKET_TTL"
	STREAM_MAX_AGE              = "STREAM_MAX_AGE"
	STREAM_HEARTBEAT            = "STREAM_HEARTBEAT"
	STREAM_CONCURRENT_MAX       = "STREAM_CONCURRENT_MAX"
	STREAM_MAX_PER_POD          = "STREAM_MAX_PER_POD"
	STREAM_LEASE_TTL            = "STREAM_LEASE_TTL"
	STREAM_GRACE                = "STREAM_GRACE"
)

// Value constants.
const (
	ServiceName = "initiative-api"

	// DefaultSessionMembersMax is the maximum of a session's member limit when SESSION_MEMBERS_MAX
	// is unset; it matches the entitlements catalog's maximum for members_per_session.
	DefaultSessionMembersMax = 20

	// CorrelationHeader carries the request correlation ID in and out of the service.
	CorrelationHeader = "X-Correlation-ID"
)

// Event stream defaults. MaxStreamTicketTTL is fixed by the ticket rows' TTL index.
const (
	DefaultStreamTicketTTL = 30 * time.Second
	MaxStreamTicketTTL     = 2 * time.Minute
	DefaultStreamMaxAge    = time.Hour
	DefaultStreamHeartbeat = 20 * time.Second
)
