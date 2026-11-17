package server

import (
	"errors"
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
	g.GET("/sessions/:id/encounters", h.list)
	g.POST("/sessions/:id/encounters", h.create)
	g.GET("/sessions/:id/encounters/:eid", h.get)
	g.GET("/sessions/:id/encounters/:eid/combatants/:cid", h.combatant)
	g.PUT("/sessions/:id/encounters/:eid/combatants/:cid/controllers", h.setControllers)
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

// create makes an encounter in the session, owned by its GM. The session decides who may and
// whether the GM's saved-encounters limit has room; the encounter service then runs the same
// validation as for a private encounter, and the response is built for the caller's role.
func (h encounterViewHandlers) create(c *gin.Context) {
	var b encounterCreate
	if !bindJSON(c, &b) {
		return
	}
	if b.Name == nil {
		badRequest(c, "request body must be a JSON encounter with a name")
		return
	}
	ctx, actor, sessionID := c.Request.Context(), authz.Viewer(c), c.Param("id")
	owner, err := h.sessions.AuthorizeEncounterCreate(ctx, actor, sessionID)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	e := b.encounter()
	for _, cb := range b.Combatants {
		e.Combatants = append(e.Combatants, cb.combatant())
	}
	enc, err := h.svc.CreateInSession(ctx, owner, actor, sessionID, e)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	v, ok := h.viewer(c)
	if !ok {
		return
	}
	view, err := h.svc.ViewForSession(ctx, v, enc.ID)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	writeView(c, http.StatusCreated, view, nil)
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
	writeView(c, http.StatusOK, view, nil)
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
func writeView(c *gin.Context, status int, view domain.EncounterView, fields []string) {
	if tag, ok := view[domain.FieldViewTag].(string); ok {
		c.Header("ETag", `"`+tag+`"`)
	} else if rev, ok := view["revision"].(int64); ok {
		c.Header("ETag", `"r`+strconv.FormatInt(rev, 10)+`"`)
	}
	c.JSON(status, projected(view, fields))
}

type controllersBody struct {
	ControllerUserIDs []string `json:"controller_user_ids"`
	Revision          int64    `json:"revision"`
}

// setControllers assigns a combatant's controllers. Only the session's GM and co-GMs may; every
// listed user must be a session member who can control (not an observer). The revision comes from
// If-Match, the query, or the body, and a stale one is a 409 carrying the current view.
func (h encounterViewHandlers) setControllers(c *gin.Context) {
	var b controllersBody
	if !bindJSON(c, &b) {
		return
	}
	if b.ControllerUserIDs == nil {
		badRequest(c, "controller_user_ids is required (an empty list clears the controllers)")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	actor := authz.Viewer(c)
	view, err := h.svc.AssignControllers(c.Request.Context(), h.sessions, actor, c.Param("id"), c.Param("eid"), c.Param("cid"), b.ControllerUserIDs, rev)
	if err != nil {
		var current any
		if errors.Is(err, data.ErrRevisionConflict) {
			if v, viewerr := h.sessions.Viewer(c.Request.Context(), actor, c.Param("id")); viewerr == nil {
				if cur, cerr := h.svc.ViewForSession(c.Request.Context(), v, c.Param("eid")); cerr == nil {
					current = cur
				}
			}
		}
		writeError(c, err, current)
		return
	}
	writeView(c, http.StatusOK, view, nil)
}
