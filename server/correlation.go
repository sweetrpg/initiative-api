package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/gin-gonic/gin"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/initiative-api/constants"
)

const correlationKey = "correlation_id"

// Correlation reuses the caller's X-Correlation-ID or mints one, and echoes it on the response
// so a request can be followed across services and log lines.
func Correlation() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(constants.CorrelationHeader)
		if id == "" || len(id) > 128 {
			id = newID()
		}
		c.Set(correlationKey, id)
		c.Header(constants.CorrelationHeader, id)
		sloggin.AddCustomAttributes(c, slog.String("correlation_id", id))
		c.Next()
	}
}

// CorrelationID returns the request's correlation ID, or "" outside the Correlation middleware.
func CorrelationID(c *gin.Context) string {
	return c.GetString(correlationKey)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
