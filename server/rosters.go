package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

type rosterHandlers struct {
	svc *domain.Rosters
	ent *entitlements.Client
}

// setupRosterHandlers registers rosters and their characters. They sit in the group that accepts
// integration credentials (read scope for GET, write scope otherwise) because integrations use the
// same endpoints as first-party clients.
func setupRosterHandlers(g *gin.RouterGroup, svc *domain.Rosters, ent *entitlements.Client) {
	h := rosterHandlers{svc, ent}
	g.GET("/rosters", h.list)
	g.POST("/rosters", h.create)
	g.GET("/rosters/:id", h.get)
	g.PATCH("/rosters/:id", h.update)
	g.DELETE("/rosters/:id", h.remove)
	g.POST("/rosters/:id/characters", h.addCharacter)
	g.PUT("/rosters/:id/characters/:cid", h.replaceCharacter)
	g.DELETE("/rosters/:id/characters/:cid", h.removeCharacter)
}

type characterBody struct {
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Notes        string            `json:"notes"`
	Health       map[string]int    `json:"health"`
	ExternalRefs map[string]string `json:"external_refs"`
}

func (b characterBody) character() data.Character {
	return data.Character{Name: b.Name, Kind: b.Kind, Notes: b.Notes, Health: b.Health, ExternalRefs: b.ExternalRefs}
}

type rosterBody struct {
	Revision   int64           `json:"revision"`
	Name       *string         `json:"name"`
	Favorite   *bool           `json:"favorite"`
	Characters []characterBody `json:"characters"`
}

// conflict writes err, attaching the roster's current state to a revision conflict so the client
// can merge and retry.
func (h rosterHandlers) conflict(c *gin.Context, err error, id string) {
	var current any
	if errors.Is(err, data.ErrRevisionConflict) {
		if r, gerr := h.svc.Get(c.Request.Context(), authz.Viewer(c), id); gerr == nil {
			current = r
		}
	}
	writeError(c, err, current)
}

func (h rosterHandlers) list(c *gin.Context) {
	q, ok := parseQuery(c, map[string]string{"favorite": typeBool})
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

func (h rosterHandlers) get(c *gin.Context) {
	r, err := h.svc.Get(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, r)
}

func (h rosterHandlers) create(c *gin.Context) {
	var b rosterBody
	if err := c.ShouldBindJSON(&b); err != nil || b.Name == nil {
		badRequest(c, "request body must be a JSON roster with a name")
		return
	}
	if !requireRoom(c, h.ent, domain.KeySavedRosters, h.svc.Count) {
		return
	}
	chars := make([]data.Character, 0, len(b.Characters))
	for _, ch := range b.Characters {
		chars = append(chars, ch.character())
	}
	owner := authz.Viewer(c)
	r, err := h.svc.Create(c.Request.Context(), owner, owner, *b.Name, b.Favorite != nil && *b.Favorite, chars)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, r)
}

func (h rosterHandlers) update(c *gin.Context) {
	var b rosterBody
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	owner, id := authz.Viewer(c), c.Param("id")
	cur, err := h.svc.Get(c.Request.Context(), owner, id)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	name, fav := cur.Name, cur.Favorite
	if b.Name != nil {
		name = *b.Name
	}
	if b.Favorite != nil {
		fav = *b.Favorite
	}
	r, err := h.svc.Rename(c.Request.Context(), owner, owner, id, rev, name, fav)
	if err != nil {
		h.conflict(c, err, id)
		return
	}
	c.JSON(http.StatusOK, r)
}

func (h rosterHandlers) remove(c *gin.Context) {
	rev, ok := revisionOf(c, 0)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	if err := h.svc.Delete(c.Request.Context(), owner, owner, c.Param("id"), rev); err != nil {
		h.conflict(c, err, c.Param("id"))
		return
	}
	c.Status(http.StatusNoContent)
}

// characterRequest is a character body plus the roster revision it is based on.
type characterRequest struct {
	characterBody
	Revision int64 `json:"revision"`
}

func (h rosterHandlers) addCharacter(c *gin.Context) {
	var b characterRequest
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be a JSON character")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	r, err := h.svc.AddCharacter(c.Request.Context(), owner, owner, c.Param("id"), rev, b.character())
	if err != nil {
		h.conflict(c, err, c.Param("id"))
		return
	}
	c.JSON(http.StatusCreated, r)
}

func (h rosterHandlers) replaceCharacter(c *gin.Context) {
	var b characterRequest
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be a JSON character")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	r, err := h.svc.UpdateCharacter(c.Request.Context(), owner, owner, c.Param("id"), rev, c.Param("cid"), b.character())
	if err != nil {
		h.conflict(c, err, c.Param("id"))
		return
	}
	c.JSON(http.StatusOK, r)
}

func (h rosterHandlers) removeCharacter(c *gin.Context) {
	rev, ok := revisionOf(c, 0)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	r, err := h.svc.RemoveCharacter(c.Request.Context(), owner, owner, c.Param("id"), rev, c.Param("cid"))
	if err != nil {
		h.conflict(c, err, c.Param("id"))
		return
	}
	c.JSON(http.StatusOK, r)
}
