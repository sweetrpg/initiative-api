package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

type changeRequestHandlers struct{ svc *domain.ChangeRequests }

// setupChangeRequestHandlers registers change requests on a session's combatants. They sit in the
// user-only group like the session routes: integration credentials never reach them. The
// responses are request records, never encounters, so there is nothing for the view builder to
// redact here; the encounter itself still changes only through UpdateInSession.
func setupChangeRequestHandlers(g *gin.RouterGroup, svc *domain.ChangeRequests) {
	h := changeRequestHandlers{svc}
	g.POST("/sessions/:id/encounters/:eid/combatants/:cid/change-requests", h.submit)
	g.GET("/sessions/:id/encounters/:eid/change-requests", h.list)
	g.POST("/sessions/:id/encounters/:eid/change-requests/:rid/approve", h.approve)
	g.POST("/sessions/:id/encounters/:eid/change-requests/:rid/reject", h.reject)
}

type changeBody struct {
	Health     map[string]int    `json:"health"`
	Conditions *[]data.Condition `json:"conditions"`
}

func (h changeRequestHandlers) submit(c *gin.Context) {
	var b changeBody
	if !bindJSON(c, &b) {
		return
	}
	req, err := h.svc.Submit(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("eid"), c.Param("cid"),
		domain.ChangeInput{Health: b.Health, Conditions: b.Conditions})
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, req)
}

func (h changeRequestHandlers) list(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	status := c.Query("status")
	if status != "" && status != data.ChangePending && status != data.ChangeApproved && status != data.ChangeRejected {
		badRequest(c, "status must be pending, approved or rejected")
		return
	}
	p, err := h.svc.List(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("eid"), status, q)
	writePage(c, p.Items, p.NextCursor, err)
}

func (h changeRequestHandlers) approve(c *gin.Context) {
	req, err := h.svc.Approve(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("eid"), c.Param("rid"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, req)
}

func (h changeRequestHandlers) reject(c *gin.Context) {
	var b struct {
		Reason string `json:"reason"`
	}
	if !bindJSON(c, &b) {
		return
	}
	req, err := h.svc.Reject(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("eid"), c.Param("rid"), b.Reason)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, req)
}
