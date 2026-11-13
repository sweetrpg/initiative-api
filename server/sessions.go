package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
)

type sessionHandlers struct{ svc *domain.Sessions }

// setupSessionHandlers registers the session routes. They sit in the user-only group: session
// membership belongs to a person, so an integration credential cannot reach it.
func setupSessionHandlers(g *gin.RouterGroup, svc *domain.Sessions) {
	h := sessionHandlers{svc}
	g.POST("/sessions", h.create)
	g.GET("/sessions", h.list)
	g.POST("/sessions/join", h.join)
	g.GET("/sessions/:id", h.get)
	g.DELETE("/sessions/:id", h.remove)
	g.POST("/sessions/:id/leave", h.leave)
	g.GET("/sessions/:id/members", h.members)
	g.PATCH("/sessions/:id/members/:user", h.setRole)
	g.DELETE("/sessions/:id/members/:user", h.removeMember)
	g.POST("/sessions/:id/invites", h.createInvite)
	g.GET("/sessions/:id/invites", h.invites)
	g.DELETE("/sessions/:id/invites/:invite", h.revokeInvite)
	g.GET("/sessions/:id/requests", h.requests)
	g.POST("/sessions/:id/requests/:request/approve", h.approve)
	g.POST("/sessions/:id/requests/:request/reject", h.reject)
	g.GET("/sessions/:id/blocks", h.blocks)
	g.POST("/sessions/:id/blocks", h.block)
	g.DELETE("/sessions/:id/blocks/:user", h.unblock)
}

func (h sessionHandlers) create(c *gin.Context) {
	var b struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with a name")
		return
	}
	s, err := h.svc.Create(c.Request.Context(), authz.Viewer(c), b.Name)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, s)
}

func (h sessionHandlers) list(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	page, err := h.svc.List(c.Request.Context(), authz.Viewer(c), q)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": page.Items, "next_cursor": page.NextCursor})
}

func (h sessionHandlers) get(c *gin.Context) {
	s, err := h.svc.Get(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, s)
}

func (h sessionHandlers) remove(c *gin.Context) {
	rev, err := strconv.ParseInt(c.Query("revision"), 10, 64)
	if err != nil || rev < 1 {
		badRequest(c, "revision query parameter is required")
		return
	}
	if err := h.svc.Delete(c.Request.Context(), authz.Viewer(c), c.Param("id"), rev); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h sessionHandlers) leave(c *gin.Context) {
	if err := h.svc.Leave(c.Request.Context(), authz.Viewer(c), c.Param("id")); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func writePage(c *gin.Context, items any, next string, err error) {
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": next})
}

func (h sessionHandlers) members(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	p, err := h.svc.Members(c.Request.Context(), authz.Viewer(c), c.Param("id"), q)
	writePage(c, p.Items, p.NextCursor, err)
}

func (h sessionHandlers) invites(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	p, err := h.svc.Invites(c.Request.Context(), authz.Viewer(c), c.Param("id"), q)
	writePage(c, p.Items, p.NextCursor, err)
}

func (h sessionHandlers) requests(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	p, err := h.svc.Requests(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Query("status") != "all", q)
	writePage(c, p.Items, p.NextCursor, err)
}

func (h sessionHandlers) blocks(c *gin.Context) {
	q, ok := pageQuery(c)
	if !ok {
		return
	}
	p, err := h.svc.Blocks(c.Request.Context(), authz.Viewer(c), c.Param("id"), q)
	writePage(c, p.Items, p.NextCursor, err)
}

func (h sessionHandlers) setRole(c *gin.Context) {
	var b struct {
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with a role")
		return
	}
	m, err := h.svc.SetRole(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("user"), b.Role)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (h sessionHandlers) removeMember(c *gin.Context) {
	if err := h.svc.Remove(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("user")); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

// createInvite returns the code once; only its hash is stored.
func (h sessionHandlers) createInvite(c *gin.Context) {
	var b struct {
		TTLHours int `json:"ttl_hours"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&b); err != nil || b.TTLHours < 0 {
			badRequest(c, "request body must be JSON with an optional ttl_hours")
			return
		}
	}
	in, code, err := h.svc.CreateInvite(c.Request.Context(), authz.Viewer(c), c.Param("id"), time.Duration(b.TTLHours)*time.Hour)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"invite": in, "code": code})
}

func (h sessionHandlers) revokeInvite(c *gin.Context) {
	in, err := h.svc.RevokeInvite(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("invite"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, in)
}

// join answers with the request's id and state only: a pending requester sees no session data.
func (h sessionHandlers) join(c *gin.Context) {
	var b struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Code == "" {
		badRequest(c, "request body must be JSON with a code")
		return
	}
	jr, err := h.svc.Join(c.Request.Context(), authz.Viewer(c), b.Code)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": jr.ID, "status": jr.Status})
}

func (h sessionHandlers) approve(c *gin.Context) {
	var b struct {
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with a role")
		return
	}
	m, err := h.svc.Approve(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("request"), b.Role)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (h sessionHandlers) reject(c *gin.Context) {
	var b struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with a reason")
		return
	}
	jr, err := h.svc.Reject(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("request"), b.Reason)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, jr)
}

func (h sessionHandlers) block(c *gin.Context) {
	var b struct {
		UserID string `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with a user_id")
		return
	}
	if err := h.svc.Block(c.Request.Context(), authz.Viewer(c), c.Param("id"), b.UserID); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h sessionHandlers) unblock(c *gin.Context) {
	if err := h.svc.Unblock(c.Request.Context(), authz.Viewer(c), c.Param("id"), c.Param("user")); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}
