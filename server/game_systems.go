package server

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

type gameSystemHandlers struct {
	systems *data.SystemRepo
	ent     *entitlements.Client
}

// setupGameSystemHandlers registers the read-only game-system resource. It sits in the group that
// accepts integration credentials (read scope).
func setupGameSystemHandlers(g *gin.RouterGroup, systems *data.SystemRepo, ent *entitlements.Client) {
	h := gameSystemHandlers{systems, ent}
	g.GET("/game-systems", privateNoStore, h.list)
	g.GET("/game-systems/:id", privateNoStore, h.get)
}

// privateNoStore keeps a shared cache from serving one caller's entitlement-dependent answer to
// another.
func privateNoStore(c *gin.Context) { c.Header("Cache-Control", "private, no-store") }

// gameSystemView is what a client receives: the definition and nothing about how it is stored
// (no revision, hash or audit fields).
type gameSystemView struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Family  string              `json:"family"`
	Aliases []string            `json:"aliases"`
	Gated   bool                `json:"gated"`
	Fields  []data.TrackerField `json:"fields"`
}

func viewOf(s data.GameSystem) gameSystemView {
	aliases, fields := s.Aliases, s.Fields
	if aliases == nil {
		aliases = []string{}
	}
	if fields == nil {
		fields = []data.TrackerField{}
	}
	return gameSystemView{ID: s.ID, Name: s.Name, Family: s.Family, Aliases: aliases, Gated: s.Gated, Fields: fields}
}

// gameSystemFields are the names ?fields= may select.
var gameSystemFields = []string{"id", "name", "family", "aliases", "gated", "fields"}

// entitled reports whether the caller holds initiative.game_systems_extra, under policy. degraded
// is true when the answer is not a fresh known value (outage, no value, or an expired cached one).
// A list passes FailOpen: anything short of a known "yes" reads as "no" and the caller sees No
// System rather than an error. A get of a gated system passes FailClosed: an outage is the typed
// 503, with no stale fallback, so a revoked or newly bought flag is never answered from cache.
func (h gameSystemHandlers) entitled(c *gin.Context, policy entitlements.Policy) (flag, degraded bool, err error) {
	res, err := checkerFor(c, h.ent).Check(c.Request.Context(), domain.KeyGameSystemsExtra, policy)
	switch {
	case err != nil:
		return false, true, err
	case res.Degraded:
		return false, true, entitlements.ErrUnavailable
	}
	return res.Flag, res.Stale, nil
}

func (h gameSystemHandlers) list(c *gin.Context) {
	q, ok := parseQuery(c, map[string]string{"gated": typeBool})
	if !ok || !checkFields(c, q.Fields, gameSystemFields) {
		return
	}
	fields := q.Fields
	q.Fields = nil
	entitled, degraded, err := h.entitled(c, entitlements.FailOpen)
	if err != nil {
		slog.WarnContext(c.Request.Context(), "entitlement unknown for the game-system list; listing No System only", "error", err)
	}
	if degraded {
		c.Header("X-Entitlements-Degraded", "true")
	}
	page, err := h.systems.ListAvailable(c.Request.Context(), q, entitled)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, s := range page.Items {
		items = append(items, projectMap(toMap(viewOf(s)), fields, "id"))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": page.NextCursor})
}

// get resolves an ID or an alias (a Foundry system ID, for one). A gated system is refused to a
// caller without the flag with the typed entitlement error; when the flag cannot be read at all
// the answer is the typed 503, never a guess.
func (h gameSystemHandlers) get(c *gin.Context) {
	fields, ok := fieldsOf(c, gameSystemFields)
	if !ok {
		return
	}
	id := c.Param("id")
	if err := data.CheckText("game system id", id, data.MaxNameLen); err != nil {
		writeError(c, data.ErrNotFound, nil)
		return
	}
	sys, err := h.systems.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	if sys.Gated {
		entitled, _, err := h.entitled(c, entitlements.FailClosed)
		if err != nil {
			writeError(c, err, nil)
			return
		}
		if !entitled {
			writeError(c, &domain.EntitlementRequiredError{Key: domain.KeyGameSystemsExtra}, nil)
			return
		}
	}
	c.JSON(http.StatusOK, projectMap(toMap(viewOf(sys)), fields, "id"))
}
