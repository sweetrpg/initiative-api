package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/initiative-api/constants"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Middleware is the chain every request runs first, in the order that matters: correlation, then
// ticket redaction (before anything that logs the URL), then the access log, recovery and tracing.
// main and the tests build the router from this one list, so the order cannot drift apart.
func Middleware(accessLog *slog.Logger, requestTimeout time.Duration) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		Correlation(),
		RedactTicket(),
		sloggin.New(accessLog),
		gin.Recovery(),
		RequestDeadline(requestTimeout),
		otelgin.Middleware(constants.ServiceName),
	}
}

// RequestDeadline bounds every request but the event stream's upgrade (a long-lived connection by
// design) to d, so a slow dependency cannot hold a request, and the goroutine behind it, open. A
// store call that runs past it returns context.DeadlineExceeded, which the error mapper reports as
// 503 timeout.
func RequestDeadline(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d <= 0 || isStreamUpgrade(c.Request) {
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
