package stream

import (
	"context"
	"log/slog"
	"sync"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

// emission is one committed change on its way to its subscribers. It renders the after-document
// for a viewer at most once per distinct view (the GM level needs no rendering; the others are
// keyed by level, plus the actor for a player, because a player sees a hidden combatant they
// control), and is dropped when the fan-out ends, so no snapshot outlives it.
type emission struct {
	ctx context.Context
	r   Renderer
	c   data.Change
	id  string

	// The tracker definition is looked up once per emission (after, and before only if the game
	// system changed), so fan-out does no per-viewer reads.
	afterDef, beforeDef   []data.TrackerField
	afterOnce, beforeOnce sync.Once

	mu    sync.Mutex
	views map[viewKey]*viewInfo
}

func (em *emission) defAfter() []data.TrackerField {
	em.afterOnce.Do(func() { em.afterDef = em.r.Definition(em.ctx, em.c.After) })
	return em.afterDef
}

func (em *emission) defBefore() []data.TrackerField {
	if em.c.Before == nil || em.c.Before.GameSystemID == em.c.After.GameSystemID {
		return em.defAfter()
	}
	em.beforeOnce.Do(func() { em.beforeDef = em.r.Definition(em.ctx, *em.c.Before) })
	return em.beforeDef
}

type viewKey struct {
	level data.ViewLevel
	actor string
}

// viewInfo is what one viewer's rendering of the encounter shows of the ids an event can name.
type viewInfo struct {
	visible map[string]bool
	current string
	tag     string
	// beforeTag is the tag of the view before the change; known is false when the write path had no
	// before-document, and then the change cannot be shown to leave the view alone.
	beforeTag string
	known     bool
	err       error
}

// unchanged reports whether the change left this viewer's view as it was.
func (vi *viewInfo) unchanged() bool { return vi.err == nil && vi.known && vi.tag == vi.beforeTag }

func newEmission(ctx context.Context, r Renderer, c data.Change) *emission {
	return &emission{ctx: ctx, r: r, c: c, id: newEventID(), views: map[viewKey]*viewInfo{}}
}

func (em *emission) info(v data.Viewer) *viewInfo {
	k := viewKey{level: v.Level()}
	if v.Level() == data.ViewPlayer {
		k.actor = v.Actor()
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	if vi, ok := em.views[k]; ok {
		return vi
	}
	vi := &viewInfo{visible: map[string]bool{}}
	view, err := domain.BuildView(em.c.After, em.defAfter(), v)
	if err != nil {
		vi.err = err
		slog.WarnContext(em.ctx, "rendering an encounter for an event subscriber failed; sending it without ids", "encounter", em.c.After.ID, "error", err)
	} else {
		list, _ := view["combatants"].([]map[string]any)
		for _, c := range list {
			if id, ok := c["id"].(string); ok {
				vi.visible[id] = true
			}
		}
		vi.current, _ = view["current_combatant_id"].(string)
		vi.tag, _ = view[domain.FieldViewTag].(string)
		if b := em.c.Before; b != nil {
			if bv, berr := domain.BuildView(*b, em.defBefore(), v); berr == nil {
				vi.beforeTag, _ = bv[domain.FieldViewTag].(string)
				vi.known = true
			}
		}
	}
	em.views[k] = vi
	return vi
}

// idEvent reports whether t names changed combatants.
func idEvent(t string) bool {
	switch t {
	case data.EventCombatantAdded, data.EventCombatantUpdated, data.EventCombatantRemoved, data.EventCombatantRestored, data.EventControllers:
		return true
	}
	return false
}

// stateEvent reports whether t carries the encounter's status, round and current combatant.
func stateEvent(t string) bool {
	switch t {
	case data.EventTurn, data.EventStarted, data.EventEnded, data.EventReset:
		return true
	}
	return false
}

// forViewer is the event v may receive for the change, or false if none. It fails closed: when
// the viewer's rendering cannot be had the event is the generic encounter.updated with no ids,
// never the unfiltered one.
func (em *emission) forViewer(v data.Viewer) (Event, bool) {
	c := em.c
	ev := base(c, em.id)
	if c.Origin.Kind == data.OriginUser && c.Origin.ClientID != "" && v.Actor() != "" && v.Actor() == c.Origin.ActorID {
		ev.Origin.ClientID = c.Origin.ClientID
	}
	switch {
	case v.Level() == data.ViewNone:
		return Event{}, false
	case c.GMOnly:
		// A lock or inactive flag is limit and entitlement state; only the GM hears of it.
		if v.Level() != data.ViewGM {
			return Event{}, false
		}
		ev = typed(ev, data.EventUpdated)
		ev.Revision = c.After.Revision
		return ev, true
	case v.Level() == data.ViewGM:
		return em.gmEvent(ev), true
	}
	if c.Type == data.EventCreated || c.Type == data.EventDeleted || c.Type == data.EventReleased {
		// These name no combatant and say the encounter itself appeared or went (a released
		// encounter no longer belongs to the viewer's session, so it is not rendered).
		return typed(ev, c.Type), true
	}
	vi := em.info(v)
	if vi.unchanged() {
		// A change the viewer cannot see: no event, no gap to explain, nothing to refetch.
		return Event{}, false
	}
	ev.ViewTag = vi.tag
	return em.filtered(ev, vi), true
}

func (em *emission) gmEvent(ev Event) Event {
	c := em.c
	ev = typed(ev, c.Type)
	ev.Revision = c.After.Revision
	switch {
	case idEvent(c.Type):
		if ids := ids(c); len(ids) > 0 {
			ev.Data["combatant_ids"] = ids
		} else {
			ev = typed(ev, data.EventUpdated)
		}
	case stateEvent(c.Type):
		ev.Data["status"], ev.Data["round"] = c.After.Status, c.After.Round
		if c.After.CurrentCombatantID != "" {
			ev.Data["current_combatant_id"] = c.After.CurrentCombatantID
		}
	}
	return ev
}

// filtered is the event for a non-GM viewer: only ids the viewer's own rendering shows, and the
// current combatant only if the viewer can see it. A change that touched nothing the viewer can
// see becomes the generic encounter.updated with no ids, so the viewer refetches and a hidden
// combatant's id, name, health, notes and initiative never appear.
func (em *emission) filtered(ev Event, vi *viewInfo) Event {
	c := em.c
	if vi.err != nil {
		return typed(ev, data.EventUpdated)
	}
	ev = typed(ev, c.Type)
	switch {
	case idEvent(c.Type):
		var shown []string
		for _, id := range ids(c) {
			if vi.visible[id] {
				shown = append(shown, id)
			}
		}
		if len(shown) == 0 {
			return typed(ev, data.EventUpdated)
		}
		ev.Data["combatant_ids"] = shown
	case stateEvent(c.Type):
		ev.Data["status"], ev.Data["round"] = c.After.Status, c.After.Round
		if vi.current != "" {
			ev.Data["current_combatant_id"] = vi.current
		}
	}
	return ev
}

// busEvent is the form published to the platform bus: the observer-level rendering of the change,
// with no client id, and nothing at all for a GM-only change. Any consumer may receive it, so it
// is as restricted as a player's.
func (em *emission) busEvent() (Event, bool) {
	if em.c.GMOnly {
		return Event{}, false
	}
	ev := base(em.c, em.id)
	ev.Origin.ClientID = ""
	if em.c.Type == data.EventCreated || em.c.Type == data.EventDeleted || em.c.Type == data.EventReleased {
		return typed(ev, em.c.Type), true
	}
	vi := em.info(data.BusViewer(em.c.After.SessionID))
	if vi.unchanged() {
		return Event{}, false
	}
	ev.ViewTag = vi.tag
	return em.filtered(ev, vi), true
}
