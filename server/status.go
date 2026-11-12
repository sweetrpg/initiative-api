package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	apicores "github.com/sweetrpg/api-core.go/server"
	"github.com/sweetrpg/api-core.go/vo"
)

// Ping is the shallow liveness and readiness probe target (PADR-0017).
//
//	@Summary	Ping
//	@Tags		status
//	@Produce	json
//	@Success	200	{object}	vo.PingResponseVO
//	@Router		/status/ping [get]
func ping(c *gin.Context) {
	c.JSON(http.StatusOK, apicores.PingHandler())
}

// Health is the deep dependency check. The service has no dependencies until the data layer
// lands, so it reports healthy; add database and cache checks there.
//
//	@Summary	Health check
//	@Tags		status
//	@Produce	json
//	@Success	200	{object}	vo.HealthResponseVO
//	@Router		/status/health [get]
func health(c *gin.Context) {
	c.JSON(http.StatusOK, vo.HealthResponseVO{Messages: []string{}, Errors: 0})
}

func setupStatusHandlers(r *gin.Engine) {
	r.GET("/status/ping", ping)
	r.GET("/status/health", health)
}
