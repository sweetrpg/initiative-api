// Package constants holds service-wide names and environment variable keys.
package constants

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
