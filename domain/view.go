package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
)

// EncounterView is an encounter as one caller may see it. It is produced only by BuildView, the
// single place that decides visibility (initiative-api-resources, "Role-aware responses"); REST
// handlers and, later, event payloads serialize nothing else. A field reaches a view only when
// the rule tables below name it and the viewer's level allows it, so a field added to a data type
// is invisible to everyone but the GM-level views until someone decides otherwise (and the test
// in view_test.go fails until they do).
type EncounterView map[string]any

// encounterRules maps each data.Encounter JSON field to the least level that sees it. Fields
// computed per viewer (see computedEncounter) are listed here too, but their raw value is never used.
var encounterRules = map[string]data.ViewLevel{
	"id": data.ViewObserver, "name": data.ViewObserver, "game_system_id": data.ViewObserver,
	"status": data.ViewObserver, "round": data.ViewObserver, "session_id": data.ViewObserver,
	"current_combatant_id": data.ViewObserver, "turn_queue": data.ViewObserver, "combatants": data.ViewObserver,

	"owner_user_id": data.ViewGM, "revision": data.ViewGM, "ordering": data.ViewGM, "tie_breaker": data.ViewGM,
	"labels": data.ViewGM, "colorblind": data.ViewGM, "theme_id": data.ViewGM, "position": data.ViewGM,
	"inactive_at": data.ViewGM, "locked_at": data.ViewGM,
	"created_at": data.ViewGM, "created_by": data.ViewGM, "updated_at": data.ViewGM, "updated_by": data.ViewGM,
	"deleted_at": data.ViewGM, "deleted_by": data.ViewGM,
}

// An encounter-level notes field, when one is added, needs a GM-only rule here; the field test in
// view_test.go fails until it has one.

// computedEncounter fields are filled from the viewer's visible combatants, never copied.
var computedEncounter = map[string]bool{"current_combatant_id": true, "turn_queue": true, "combatants": true}

// combatantRules is the same table for data.Combatant.
var combatantRules = map[string]data.ViewLevel{
	"id": data.ViewObserver, "name": data.ViewObserver, "kind": data.ViewObserver, "conditions": data.ViewObserver,
	"health": data.ViewObserver, "notes": data.ViewObserver,

	"initiative": data.ViewGM, "removed": data.ViewGM, "source_character_id": data.ViewGM, "external_refs": data.ViewGM,
	"controller_user_ids": data.ViewGM, "hidden": data.ViewGM, "health_visibility": data.ViewGM, "notes_shared": data.ViewGM,
}

// computedCombatant fields depend on the viewer and the combatant's visibility settings.
var computedCombatant = map[string]bool{"health": true, "notes": true}

// conditionRules is the same table for data.Condition.
var conditionRules = map[string]data.ViewLevel{"name": data.ViewObserver, "rounds_remaining": data.ViewObserver}

// Derived view fields that no data type carries.
const (
	fieldHealthStatus   = "health_status"
	fieldMine           = "mine"
	fieldCombatantCount = "combatant_count"
	fieldViewTag        = "view_tag"
)

// FieldViewTag is the key of a non-GM view's digest.
const FieldViewTag = fieldViewTag

// Health statuses a non-GM sees when a combatant's health is shown as status.
const (
	StatusHealthy   = "healthy"
	StatusHurt      = "hurt"
	StatusNearDeath = "near_death"
	StatusDown      = "down"
)

// ErrNoView is returned when a view is asked of a viewer that grants nothing, or for an
// encounter outside the viewer's scope.
var ErrNoView = data.ErrForbidden

// BuildView renders enc for v. def is the encounter's game system tracker definition, used for
// the coarse health status. The viewer must come from the data layer (data.AuthorizeView or an
// owner-scoped load); BuildView does not look at membership.
func BuildView(enc data.Encounter, def []data.TrackerField, v data.Viewer) (EncounterView, error) {
	if v.Level() == data.ViewNone || enc.SessionID != v.SessionID() {
		return nil, ErrNoView
	}
	gm := v.Level() == data.ViewGM
	combatants, queue, current, count := visibleCombatants(enc, def, v)

	out := projectStruct(reflect.ValueOf(enc), encounterRules, v.Level(), func(name string) (any, bool) {
		switch name {
		case "combatants":
			return combatants, true
		case "turn_queue":
			return queue, true
		case "current_combatant_id":
			return current, current != ""
		}
		return nil, false
	}, computedEncounter)
	out[fieldCombatantCount] = count
	if !gm {
		// The stored revision moves on every edit, hidden ones included, so a non-GM gets a tag of
		// what it can see instead: it changes only when its own view does.
		out[fieldViewTag] = viewTag(out)
	}
	return out, nil
}

// Combatant returns the combatant with id from the view, if the view has it. A combatant the
// viewer may not see is not in the view, so it is simply not found.
func (v EncounterView) Combatant(id string) (map[string]any, bool) {
	list, _ := v["combatants"].([]map[string]any)
	for _, c := range list {
		if c["id"] == id {
			return c, true
		}
	}
	return nil, false
}

// visibleCombatants returns the combatant views, turn queue, current combatant ID and count the
// viewer may see. For a non-GM, hidden and removed combatants are absent, not redacted, and the
// queue and current turn are derived from what remains.
func visibleCombatants(enc data.Encounter, def []data.TrackerField, v data.Viewer) (list []map[string]any, queue []string, current string, count int) {
	gm := v.Level() == data.ViewGM
	shown := map[string]bool{}
	list = []map[string]any{}
	for _, c := range enc.Combatants {
		if !c.Removed {
			count++
		}
		// A hidden combatant stays visible to the player who controls it; nobody else sees it.
		if !gm && (c.Removed || (c.Hidden && !controls(c, v))) {
			continue
		}
		shown[c.ID] = true
		list = append(list, combatantView(c, def, v))
	}
	queue = []string{}
	for _, id := range enc.TurnQueue {
		if shown[id] {
			queue = append(queue, id)
		}
	}
	if shown[enc.CurrentCombatantID] {
		current = enc.CurrentCombatantID
	}
	if !gm {
		// The count a non-GM sees is the count it can see.
		count = len(list)
	}
	return list, queue, current, count
}

// controls reports whether the viewer is a player who controls c. An observer controls nothing,
// even if listed.
func controls(c data.Combatant, v data.Viewer) bool {
	return v.Level() == data.ViewPlayer && slices.Contains(c.ControllerUserIDs, v.Actor())
}

func combatantView(c data.Combatant, def []data.TrackerField, v data.Viewer) map[string]any {
	gm := v.Level() == data.ViewGM
	own := controls(c, v)
	out := projectStruct(reflect.ValueOf(c), combatantRules, v.Level(), func(name string) (any, bool) {
		switch name {
		case "health":
			if (gm || own || c.HealthVisibility == data.HealthFull) && len(c.Health) > 0 {
				return maps.Clone(c.Health), true
			}
		case "notes":
			if gm || c.NotesShared {
				return c.Notes, true
			}
		}
		return nil, false
	}, computedCombatant)
	if !gm && !own && c.HealthVisibility == data.HealthStatus {
		if s, ok := healthStatus(def, c.Health); ok {
			out[fieldHealthStatus] = s
		}
	}
	if v.Level() == data.ViewPlayer {
		out[fieldMine] = own
	}
	conds := make([]map[string]any, 0, len(c.Conditions))
	for _, cond := range c.Conditions {
		conds = append(conds, projectStruct(reflect.ValueOf(cond), conditionRules, v.Level(), nil, nil))
	}
	out["conditions"] = conds
	return out
}

// healthStatus is the coarse state of a combatant: from the first bounded counter in the
// definition (current over its maximum), else the first counter (down at its minimum, otherwise
// healthy). It reports false when the definition has no counter, so nothing is shown.
func healthStatus(def []data.TrackerField, health map[string]int) (string, bool) {
	var first *data.TrackerField
	for i, f := range def {
		if f.Kind != data.FieldCounter {
			continue
		}
		if first == nil {
			first = &def[i]
		}
		bound, ok := boundOf(f, health)
		if !ok {
			continue
		}
		return statusOf(health[f.Key], minOf(f), bound), true
	}
	if first == nil {
		return "", false
	}
	if health[first.Key] <= minOf(*first) {
		return StatusDown, true
	}
	return StatusHealthy, true
}

func boundOf(f data.TrackerField, health map[string]int) (int, bool) {
	switch {
	case f.Max != nil:
		return *f.Max, true
	case f.MaxRef != "":
		return health[f.MaxRef], true
	}
	return 0, false
}

func minOf(f data.TrackerField) int {
	if f.Min != nil {
		return *f.Min
	}
	return 0
}

func statusOf(cur, min, bound int) string {
	switch {
	case cur <= min || bound <= min:
		return StatusDown
	case cur >= bound:
		return StatusHealthy
	case float64(cur-min) <= 0.25*float64(bound-min):
		return StatusNearDeath
	default:
		return StatusHurt
	}
}

// projectStruct copies the fields of the struct v that rules allow at level, keyed by JSON name,
// honoring omitempty. A field with no rule is skipped (default deny). A computed field takes its
// value from compute, or is skipped when compute declines; its raw value is never read.
func projectStruct(v reflect.Value, rules map[string]data.ViewLevel, level data.ViewLevel, compute func(string) (any, bool), computed map[string]bool) map[string]any {
	out := map[string]any{}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			maps.Copy(out, projectStruct(v.Field(i), rules, level, compute, computed))
			continue
		}
		name, omitEmpty, ok := jsonName(f)
		if !ok {
			continue
		}
		min, ruled := rules[name]
		if !ruled || min > level {
			continue
		}
		if computed[name] {
			if val, ok := compute(name); ok {
				out[name] = val
			}
			continue
		}
		if omitEmpty && v.Field(i).IsZero() {
			continue
		}
		out[name] = v.Field(i).Interface()
	}
	return out
}

// jsonName returns the JSON key of f, whether it is omitempty, and false for unexported or "-" fields.
func jsonName(f reflect.StructField) (name string, omitEmpty, ok bool) {
	if !f.IsExported() {
		return "", false, false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	return name, slices.Contains(parts[1:], "omitempty"), true
}

// viewTag is a short digest of a non-GM view, for ETags and change detection.
func viewTag(view map[string]any) string {
	b, err := json.Marshal(view) // map keys marshal sorted, so equal views give equal tags
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// ViewOwned renders the owner's private encounter. An encounter that belongs to a session is
// reached through ViewForSession.
func (s *Encounters) ViewOwned(ctx context.Context, owner, id string) (EncounterView, error) {
	enc, v, err := s.repo.LoadOwnedView(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	return s.render(ctx, enc, v)
}

// ViewForSession renders one of the viewer's session's encounters. A missing encounter and one in
// another session are the same error.
func (s *Encounters) ViewForSession(ctx context.Context, v data.Viewer, id string) (EncounterView, error) {
	enc, err := s.repo.LoadForViewer(ctx, v, id)
	if err != nil {
		return nil, err
	}
	return s.render(ctx, enc, v)
}

// ViewsForSession renders the viewer's session's encounters in display order.
func (s *Encounters) ViewsForSession(ctx context.Context, v data.Viewer, limit int64) ([]EncounterView, error) {
	encs, err := s.repo.ListForViewer(ctx, v, limit)
	if err != nil {
		return nil, err
	}
	out := make([]EncounterView, 0, len(encs))
	for _, enc := range encs {
		view, err := s.render(ctx, enc, v)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// render looks up the encounter's tracker definition and builds the view. A system with no
// definition leaves the status out; it never widens what is shown.
func (s *Encounters) render(ctx context.Context, enc data.Encounter, v data.Viewer) (EncounterView, error) {
	var def []data.TrackerField
	sys, err := s.systems.GetOrDefault(ctx, enc.GameSystemID)
	switch {
	case err == nil:
		def = sys.Fields
	case !errors.Is(err, data.ErrNotFound):
		return nil, err
	}
	return BuildView(enc, def, v)
}

// ViewsOwned renders one page of the owner's private encounters.
func (s *Encounters) ViewsOwned(ctx context.Context, owner string, q data.Query) (data.Page[EncounterView], error) {
	page, v, err := s.repo.ListOwnedViews(ctx, owner, q)
	if err != nil {
		return data.Page[EncounterView]{}, err
	}
	out := data.Page[EncounterView]{NextCursor: page.NextCursor, Items: make([]EncounterView, 0, len(page.Items))}
	for _, enc := range page.Items {
		view, err := s.render(ctx, enc, v)
		if err != nil {
			return data.Page[EncounterView]{}, err
		}
		out.Items = append(out.Items, view)
	}
	return out, nil
}
