// Package constants holds service-wide names and environment variable keys.
package constants

// Environment variable names.
const (
	LOG_LEVEL                   = "LOG_LEVEL"
	OTEL_EXPORTER_OTLP_ENDPOINT = "OTEL_EXPORTER_OTLP_ENDPOINT"
	ALLOWED_ORIGINS             = "ALLOWED_ORIGINS"
)

// Value constants.
const (
	ServiceName = "initiative-api"

	// CorrelationHeader carries the request correlation ID in and out of the service.
	CorrelationHeader = "X-Correlation-ID"
)
