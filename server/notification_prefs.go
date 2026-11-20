package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

type prefHandlers struct{ svc *domain.NotificationPrefs }

// setupNotificationPrefHandlers registers a member's per-session notification mutes. They sit in
// the user-only group: preferences belong to a person.
func setupNotificationPrefHandlers(g *gin.RouterGroup, svc *domain.NotificationPrefs) {
	h := prefHandlers{svc}
	g.GET("/sessions/:id/notification-preferences", h.get)
	g.PUT("/sessions/:id/notification-preferences", h.put)
}

type prefBody struct {
	Muted []string `json:"muted"`
}

func writePrefs(c *gin.Context, muted []string, err error) {
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"muted": muted, "types": data.NoticeTypes})
}

func (h prefHandlers) get(c *gin.Context) {
	muted, err := h.svc.Get(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	writePrefs(c, muted, err)
}

func (h prefHandlers) put(c *gin.Context) {
	var b prefBody
	if !bindJSON(c, &b) {
		return
	}
	muted, err := h.svc.Set(c.Request.Context(), authz.Viewer(c), c.Param("id"), b.Muted)
	writePrefs(c, muted, err)
}
