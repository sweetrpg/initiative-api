package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

type encounterHandlers struct {
	svc     *domain.Encounters
	rosters *domain.Rosters
	ent     *entitlements.Client
}

// setupEncounterHandlers registers the owner's private encounters: reads, writes, combatants, turn
// actions, and the roster transfers. They sit in the group that accepts integration credentials.
// Every encounter response is a domain.EncounterView written by respond, the only place these
// handlers serialize an encounter (initiative-api-resources, "Role-aware responses").
func setupEncounterHandlers(g *gin.RouterGroup, svc *domain.Encounters, rosters *domain.Rosters, ent *entitlements.Client) {
	h := encounterHandlers{svc, rosters, ent}
	g.GET("/encounters", h.list)
	g.POST("/encounters", h.create)
	g.GET("/encounters/:id", h.get)
	g.PATCH("/encounters/:id", h.update)
	g.DELETE("/encounters/:id", h.remove)
	g.POST("/encounters/:id/combatants", h.addCombatant)
	g.PATCH("/encounters/:id/combatants/:cid", h.updateCombatant)
	g.DELETE("/encounters/:id/combatants/:cid", h.removeCombatant)
	g.POST("/encounters/:id/combatants/:cid/restore", h.restoreCombatant)
	g.POST("/encounters/:id/start", h.turnAction(svc.Start))
	g.POST("/encounters/:id/end", h.turnAction(svc.End))
	g.POST("/encounters/:id/reset", h.turnAction(svc.Reset))
	g.POST("/encounters/:id/advance", h.turnAction(svc.Advance))
	g.POST("/encounters/:id/back", h.turnAction(svc.StepBack))
	if rosters != nil {
		g.POST("/rosters/:id/encounters", h.fromRoster)
		g.POST("/encounters/:id/save-as-roster", h.saveAsRoster)
	}
}

// encounterWrite is a domain write on one encounter at a revision.
type encounterWrite func(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error)

type conditionBody struct {
	Name            string `json:"name"`
	RoundsRemaining *int   `json:"rounds_remaining"`
}

type combatantBody struct {
	Name             string            `json:"name"`
	Kind             string            `json:"kind"`
	Initiative       *float64          `json:"initiative"`
	Health           map[string]int    `json:"health"`
	Conditions       []conditionBody   `json:"conditions"`
	Notes            string            `json:"notes"`
	ExternalRefs     map[string]string `json:"external_refs"`
	Hidden           bool              `json:"hidden"`
	HealthVisibility string            `json:"health_visibility"`
	NotesShared      bool              `json:"notes_shared"`
}

func conditionsOf(in []conditionBody) []data.Condition {
	out := make([]data.Condition, 0, len(in))
	for _, c := range in {
		out = append(out, data.Condition{Name: c.Name, RoundsRemaining: c.RoundsRemaining})
	}
	return out
}

func (b combatantBody) combatant() data.Combatant {
	return data.Combatant{Name: b.Name, Kind: b.Kind, Initiative: b.Initiative, Health: b.Health, Conditions: conditionsOf(b.Conditions),
		Notes: b.Notes, ExternalRefs: b.ExternalRefs, Hidden: b.Hidden, HealthVisibility: b.HealthVisibility, NotesShared: b.NotesShared}
}

// encounterRequest is an encounter body: every field optional on update, the settings plus the
// starting combatants on create.
type encounterRequest struct {
	Revision     int64           `json:"revision"`
	Name         *string         `json:"name"`
	GameSystemID *string         `json:"game_system_id"`
	Ordering     *string         `json:"ordering"`
	TieBreaker   *string         `json:"tie_breaker"`
	Labels       *bool           `json:"labels"`
	Colorblind   *bool           `json:"colorblind"`
	ThemeID      *string         `json:"theme_id"`
	Position     *int            `json:"position"`
	Combatants   []combatantBody `json:"combatants"`
}

// settings is the set of fields the request names, keyed as the data layer's update list.
func (b encounterRequest) settings() map[string]any {
	set := map[string]any{}
	put := func(k string, ok bool, v any) {
		if ok {
			set[k] = v
		}
	}
	if b.Name != nil {
		put("name", true, *b.Name)
	}
	if b.GameSystemID != nil {
		put("game_system_id", true, *b.GameSystemID)
	}
	if b.Ordering != nil {
		put("ordering", true, *b.Ordering)
	}
	if b.TieBreaker != nil {
		put("tie_breaker", true, *b.TieBreaker)
	}
	if b.Labels != nil {
		put("labels", true, *b.Labels)
	}
	if b.Colorblind != nil {
		put("colorblind", true, *b.Colorblind)
	}
	if b.ThemeID != nil {
		put("theme_id", true, *b.ThemeID)
	}
	if b.Position != nil {
		put("position", true, *b.Position)
	}
	return set
}

// encounter builds a new draft from the request, defaulting the ordering mode and tie breaker.
func (b encounterRequest) encounter() data.Encounter {
	e := data.Encounter{Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}
	if b.Name != nil {
		e.Name = *b.Name
	}
	if b.GameSystemID != nil {
		e.GameSystemID = *b.GameSystemID
	}
	if b.Ordering != nil {
		e.Ordering = *b.Ordering
	}
	if b.TieBreaker != nil {
		e.TieBreaker = *b.TieBreaker
	}
	if b.Labels != nil {
		e.Labels = *b.Labels
	}
	if b.Colorblind != nil {
		e.Colorblind = *b.Colorblind
	}
	if b.ThemeID != nil {
		e.ThemeID = *b.ThemeID
	}
	if b.Position != nil {
		e.Position = *b.Position
	}
	for _, cb := range b.Combatants {
		e.Combatants = append(e.Combatants, cb.combatant())
	}
	return e
}

// respond is the one place these handlers write an encounter: the caller's view of enc.
func (h encounterHandlers) respond(c *gin.Context, status int, enc data.Encounter) {
	view, err := h.svc.ViewWritten(c.Request.Context(), authz.Viewer(c), enc)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	writeView(c, status, view)
}

// fail writes err; a revision conflict carries the encounter's current view.
func (h encounterHandlers) fail(c *gin.Context, err error, id string) {
	var current any
	if errors.Is(err, data.ErrRevisionConflict) {
		if v, verr := h.svc.ViewOwned(c.Request.Context(), authz.Viewer(c), id); verr == nil {
			current = v
		}
	}
	writeError(c, err, current)
}

func (h encounterHandlers) list(c *gin.Context) {
	q, ok := parseQuery(c, map[string]string{"position": typeInt})
	if !ok {
		return
	}
	page, err := h.svc.ViewsOwned(c.Request.Context(), authz.Viewer(c), q)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": page.Items, "next_cursor": page.NextCursor})
}

func (h encounterHandlers) get(c *gin.Context) {
	view, err := h.svc.ViewOwned(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	writeView(c, http.StatusOK, view)
}

func (h encounterHandlers) create(c *gin.Context) {
	var b encounterRequest
	if err := c.ShouldBindJSON(&b); err != nil || b.Name == nil {
		badRequest(c, "request body must be a JSON encounter with a name")
		return
	}
	if !requireRoom(c, h.ent, domain.KeySavedEncounters, h.svc.Count) {
		return
	}
	owner := authz.Viewer(c)
	enc, err := h.svc.Create(c.Request.Context(), owner, owner, b.encounter())
	if err != nil {
		writeError(c, err, nil)
		return
	}
	h.respond(c, http.StatusCreated, enc)
}

func (h encounterHandlers) update(c *gin.Context) {
	var b encounterRequest
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	enc, err := h.svc.UpdateSettings(c.Request.Context(), owner, owner, c.Param("id"), rev, b.settings())
	if err != nil {
		h.fail(c, err, c.Param("id"))
		return
	}
	h.respond(c, http.StatusOK, enc)
}

func (h encounterHandlers) remove(c *gin.Context) {
	rev, ok := revisionOf(c, 0)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	if err := h.svc.Delete(c.Request.Context(), owner, owner, c.Param("id"), rev); err != nil {
		h.fail(c, err, c.Param("id"))
		return
	}
	c.Status(http.StatusNoContent)
}

func (h encounterHandlers) addCombatant(c *gin.Context) {
	var b struct {
		combatantBody
		Revision int64 `json:"revision"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be a JSON combatant")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	enc, err := h.svc.AddCombatant(c.Request.Context(), owner, owner, c.Param("id"), rev, b.combatant())
	if err != nil {
		h.fail(c, err, c.Param("id"))
		return
	}
	h.respond(c, http.StatusCreated, enc)
}

func (h encounterHandlers) updateCombatant(c *gin.Context) {
	var b struct {
		Revision         int64            `json:"revision"`
		Name             *string          `json:"name"`
		Kind             *string          `json:"kind"`
		Initiative       *float64         `json:"initiative"`
		Notes            *string          `json:"notes"`
		Health           map[string]int   `json:"health"`
		Conditions       *[]conditionBody `json:"conditions"`
		Hidden           *bool            `json:"hidden"`
		HealthVisibility *string          `json:"health_visibility"`
		NotesShared      *bool            `json:"notes_shared"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON")
		return
	}
	rev, ok := revisionOf(c, b.Revision)
	if !ok {
		return
	}
	patch := domain.CombatantPatch{Name: b.Name, Kind: b.Kind, Initiative: b.Initiative, Notes: b.Notes, Health: b.Health,
		Hidden: b.Hidden, HealthVisibility: b.HealthVisibility, NotesShared: b.NotesShared}
	if b.Conditions != nil {
		conds := conditionsOf(*b.Conditions)
		patch.Conditions = &conds
	}
	owner := authz.Viewer(c)
	enc, err := h.svc.UpdateCombatant(c.Request.Context(), owner, owner, c.Param("id"), rev, c.Param("cid"), patch)
	if err != nil {
		h.fail(c, err, c.Param("id"))
		return
	}
	h.respond(c, http.StatusOK, enc)
}

func (h encounterHandlers) removeCombatant(c *gin.Context) {
	h.combatantAction(c, h.svc.RemoveCombatant)
}

func (h encounterHandlers) restoreCombatant(c *gin.Context) {
	h.combatantAction(c, h.svc.RestoreCombatant)
}

// combatantAction runs a revisioned action on one combatant and returns the encounter.
func (h encounterHandlers) combatantAction(c *gin.Context, fn func(ctx context.Context, owner, actor, id string, rev int64, combatantID string) (data.Encounter, error)) {
	rev, ok := revisionOf(c, bodyRevision(c))
	if !ok {
		return
	}
	owner := authz.Viewer(c)
	enc, err := fn(c.Request.Context(), owner, owner, c.Param("id"), rev, c.Param("cid"))
	if err != nil {
		h.fail(c, err, c.Param("id"))
		return
	}
	h.respond(c, http.StatusOK, enc)
}

// turnAction serves the revisioned actions that change combat state: start, end, reset, advance
// and back. The body is optional; the revision may come from If-Match or the query.
func (h encounterHandlers) turnAction(fn encounterWrite) gin.HandlerFunc {
	return func(c *gin.Context) {
		rev, ok := revisionOf(c, bodyRevision(c))
		if !ok {
			return
		}
		owner := authz.Viewer(c)
		enc, err := fn(c.Request.Context(), owner, owner, c.Param("id"), rev)
		if err != nil {
			h.fail(c, err, c.Param("id"))
			return
		}
		h.respond(c, http.StatusOK, enc)
	}
}

// bodyRevision reads an optional JSON body's revision field, 0 when there is no usable body.
func bodyRevision(c *gin.Context) int64 {
	var b struct {
		Revision int64 `json:"revision"`
	}
	if c.Request.Body != nil {
		_ = json.NewDecoder(c.Request.Body).Decode(&b)
	}
	return b.Revision
}

// fromRoster creates an encounter with one combatant per character of the roster in the path.
func (h encounterHandlers) fromRoster(c *gin.Context) {
	var b encounterRequest
	if err := c.ShouldBindJSON(&b); err != nil || b.Name == nil {
		badRequest(c, "request body must be a JSON encounter with a name")
		return
	}
	if !requireRoom(c, h.ent, domain.KeySavedEncounters, h.svc.Count) {
		return
	}
	owner := authz.Viewer(c)
	e := b.encounter()
	e.Combatants = nil
	enc, err := h.rosters.CreateEncounterFromRoster(c.Request.Context(), owner, owner, c.Param("id"), e)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	h.respond(c, http.StatusCreated, enc)
}

// saveAsRoster stores a roster from the encounter in the path. The response is the roster, which
// holds no combatant visibility data.
func (h encounterHandlers) saveAsRoster(c *gin.Context) {
	var b struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Name == "" {
		badRequest(c, "request body must be JSON with a roster name")
		return
	}
	if !requireRoom(c, h.ent, domain.KeySavedRosters, h.rosters.Count) {
		return
	}
	owner := authz.Viewer(c)
	r, err := h.rosters.SaveAsRoster(c.Request.Context(), owner, owner, c.Param("id"), b.Name)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, r)
}
