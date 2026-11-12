package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
)

// setupRefHandlers serves the integration lookup of the caller's combatants by external reference.
func setupRefHandlers(g *gin.RouterGroup, enc *domain.Encounters) {
	g.GET("/combatants", func(c *gin.Context) {
		items, err := enc.FindByExternalRef(c.Request.Context(), authz.Viewer(c), c.Query("ref_key"), c.Query("ref_value"))
		if err != nil {
			writeError(c, err, nil)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
}
