package stream

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

// Preferences says which notice types a user has turned off for a session.
type Preferences interface {
	Muted(ctx context.Context, userID, sessionID string) ([]string, error)
}

// Managers lists the user ids of a session's GM and co-GMs.
type Managers interface {
	ManagerIDs(ctx context.Context, sessionID string) ([]string, error)
}

// SetPreferences installs the preference store consulted before an addressed event is delivered.
// Without one every addressed event is delivered.
func (h *Hub) SetPreferences(p Preferences) {
	h.mu.Lock()
	h.prefs = p
	h.mu.Unlock()
}

// SetManagers installs the lookup that addresses change-request submissions to the session's GM
// and co-GMs. Without one those submissions notify nobody.
func (h *Hub) SetManagers(m Managers) {
	h.mu.Lock()
	h.managers = m
	h.mu.Unlock()
}

// Notify implements data.Notifier. It delivers the event to each addressee's open streams of the
// notice's session (a per-encounter stream only for that encounter), skipping an addressee who
// muted the type for the session. A stream's viewer is fixed when it is granted and ends when
// membership ends, so only current members of the session receive it. It never blocks the caller.
func (h *Hub) Notify(ctx context.Context, n data.Notice) {
	if n.SessionID == "" || len(n.Users) == 0 {
		return
	}
	h.notifyLocal(ctx, n)
	if r := h.relay(); r != nil {
		r.publishNotice(n)
	}
}

// notifyLocal delivers n to this pod's streams only.
func (h *Hub) notifyLocal(ctx context.Context, n data.Notice) {
	ev := noticeEvent(n, newEventID())
	subs := h.snapshot(n.EncounterID)
	h.mu.RLock()
	prefs := h.prefs
	h.mu.RUnlock()
	done := map[string]bool{}
	for _, u := range n.Users {
		if u == "" || done[u] {
			continue
		}
		done[u] = true
		if h.muted(ctx, prefs, u, n) {
			continue
		}
		for _, s := range subs {
			if v := s.viewer.Load(); v != nil && v.Actor() == u && v.SessionID() == n.SessionID && s.send(ev) {
				s.overflowed.Store(true)
				h.dropped.Add(1)
				s.Close()
			}
		}
	}
}

// muted reports whether u turned the notice off. A failed lookup delivers: a notification is
// worth more than a missed mute, and the mute only chooses what a client shows.
func (h *Hub) muted(ctx context.Context, prefs Preferences, u string, n data.Notice) bool {
	if prefs == nil {
		return false
	}
	muted, err := prefs.Muted(ctx, u, n.SessionID)
	if err != nil {
		slog.WarnContext(ctx, "reading notification preferences failed; delivering the event", "user", u, "type", n.Type, "error", err)
		return false
	}
	return slices.Contains(muted, n.Type)
}

// noticeEvent is the wire form of n. It has no revision or view tag, which belong to the encounter
// state events and would tell a non-GM about edits it cannot see.
func noticeEvent(n data.Notice, id string) Event {
	d := n.Data
	if d == nil {
		d = map[string]any{}
	}
	return Event{
		EventID: id, OccurredAt: n.At.UTC(), Source: Source,
		EncounterID: n.EncounterID, SessionID: n.SessionID,
		EntityType: "encounter", EntityID: n.EncounterID,
		Type: n.Type, Action: actionOf(n.Type),
		Origin: Origin{Kind: data.OriginSystem}, Data: d,
	}
}

// turnNotices addresses a turn change: the new current combatant's controllers hear turn.yours and
// the next one's hear turn.next. Each carries only the combatant's id and name, which its
// controller always sees.
func turnNotices(c data.Change) []data.Notice {
	e := c.After
	if e.SessionID == "" || (c.Type != data.EventTurn && c.Type != data.EventStarted) || e.CurrentCombatantID == "" {
		return nil
	}
	var out []data.Notice
	add := func(kind, id string) {
		cb, ok := combatantOf(e, id)
		if !ok || cb.Removed || len(cb.ControllerUserIDs) == 0 {
			return
		}
		out = append(out, data.Notice{
			Type: kind, SessionID: e.SessionID, EncounterID: e.ID, Users: slices.Clone(cb.ControllerUserIDs), At: c.At,
			Data: map[string]any{"combatant_id": cb.ID, "combatant_name": cb.Name, "round": e.Round},
		})
	}
	add(data.NoticeTurnYours, e.CurrentCombatantID)
	if i := slices.Index(e.TurnQueue, e.CurrentCombatantID); i >= 0 && len(e.TurnQueue) > 1 {
		add(data.NoticeTurnNext, e.TurnQueue[(i+1)%len(e.TurnQueue)])
	}
	return out
}

func combatantOf(e data.Encounter, id string) (data.Combatant, bool) {
	i := slices.IndexFunc(e.Combatants, func(c data.Combatant) bool { return c.ID == id })
	if i < 0 {
		return data.Combatant{}, false
	}
	return e.Combatants[i], true
}

// ChangeSubmitted tells the session's GM and co-GMs a player filed a change request.
func (h *Hub) ChangeSubmitted(ctx context.Context, req data.ChangeRequest) {
	h.mu.RLock()
	m := h.managers
	h.mu.RUnlock()
	if m == nil {
		return
	}
	ids, err := m.ManagerIDs(ctx, req.SessionID)
	if err != nil {
		slog.WarnContext(ctx, "finding the session's managers failed; the change request notice was not sent", "session", req.SessionID, "error", err)
		return
	}
	h.Notify(ctx, requestNotice(data.NoticeChangeSubmitted, req, ids, req.CreatedAt))
}

// ChangeDecided tells the player who filed the request that it was approved or rejected.
func (h *Hub) ChangeDecided(ctx context.Context, req data.ChangeRequest) {
	kind := data.NoticeChangeRejected
	if req.Status == data.ChangeApproved {
		kind = data.NoticeChangeApproved
	}
	at := req.CreatedAt
	if req.DecidedAt != nil {
		at = *req.DecidedAt
	}
	h.Notify(ctx, requestNotice(kind, req, []string{req.RequestedBy}, at))
}

// requestNotice carries the ids of a change request and the decision, never the proposed values.
func requestNotice(kind string, req data.ChangeRequest, users []string, at time.Time) data.Notice {
	return data.Notice{
		Type: kind, SessionID: req.SessionID, EncounterID: req.EncounterID, Users: users, At: at,
		Data: map[string]any{"change_request_id": req.ID, "combatant_id": req.CombatantID},
	}
}
