package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

type encounterViewHandlers struct {
	svc      *domain.Encounters
	sessions *domain.Sessions
}

// setupEncounterViewHandlers registers the read routes for encounters. Every response is a
// domain.EncounterView built for the caller's role; no handler here serializes an encounter any
// other way (initiative-api-resources, "Role-aware responses"). They sit in the user-only group,
// like the session routes.
func setupEncounterViewHandlers(g *gin.RouterGroup, svc *domain.Encounters, sessions *domain.Sessions) {
	h := encounterViewHandlers{svc, sessions}
	g.GET("/encounters", h.ownedList)
	g.GET("/encounters/:id", h.owned)
	g.GET("/sessions/:id/encounters", h.list)
	g.GET("/sessions/:id/encounters/:eid", h.get)
	g.GET("/sessions/:id/encounters/:eid/combatants/:cid", h.combatant)
}

// viewer authorizes the caller on the session in the path. A caller who is not a member gets 403
// before anything about the session's contents is looked up.
func (h encounterViewHandlers) viewer(c *gin.Context) (data.Viewer, bool) {
	v, err := h.sessions.Viewer(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return data.Viewer{}, false
	}
	return v, true
}

func (h encounterViewHandlers) ownedList(c *gin.Context) {
	q := data.Query{Cursor: c.Query("cursor")}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(c, "limit must be a positive integer")
			return
		}
		q.Limit = n
	}
	page, err := h.svc.ViewsOwned(c.Request.Context(), authz.Viewer(c), q)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": page.Items, "next_cursor": page.NextCursor})
}

func (h encounterViewHandlers) owned(c *gin.Context) {
	view, err := h.svc.ViewOwned(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	writeView(c, view)
}

func (h encounterViewHandlers) list(c *gin.Context) {
	v, ok := h.viewer(c)
	if !ok {
		return
	}
	views, err := h.svc.ViewsForSession(c.Request.Context(), v, 100)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": views})
}

func (h encounterViewHandlers) get(c *gin.Context) {
	v, ok := h.viewer(c)
	if !ok {
		return
	}
	view, err := h.svc.ViewForSession(c.Request.Context(), v, c.Param("eid"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	writeView(c, view)
}

// combatant answers from the encounter view, so a combatant the caller may not see is absent from
// the lookup and gets the same 404 as one that does not exist.
func (h encounterViewHandlers) combatant(c *gin.Context) {
	v, ok := h.viewer(c)
	if !ok {
		return
	}
	view, err := h.svc.ViewForSession(c.Request.Context(), v, c.Param("eid"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	cv, found := view.Combatant(c.Param("cid"))
	if !found {
		writeError(c, data.ErrNotFound, nil)
		return
	}
	c.JSON(http.StatusOK, cv)
}

// writeView sends view with an ETag: the stored revision for a GM, and for everyone else the digest
// of what they can see, so a hidden edit never changes what a player's client observes.
func writeView(c *gin.Context, view domain.EncounterView) {
	if tag, ok := view[domain.FieldViewTag].(string); ok {
		c.Header("ETag", `"`+tag+`"`)
	} else if rev, ok := view["revision"].(int64); ok {
		c.Header("ETag", `"r`+strconv.FormatInt(rev, 10)+`"`)
	}
	c.JSON(http.StatusOK, view)
}
