package server

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/initiative-api/constants"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Middleware is the chain every request runs first, in the order that matters: correlation, then
// ticket redaction (before anything that logs the URL), then the access log, recovery and tracing.
// main and the tests build the router from this one list, so the order cannot drift apart.
func Middleware(accessLog *slog.Logger) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		Correlation(),
		RedactTicket(),
		sloggin.New(accessLog),
		gin.Recovery(),
		otelgin.Middleware(constants.ServiceName),
	}
}
