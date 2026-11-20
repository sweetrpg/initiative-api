package stream

import (
	"context"
	"log/slog"
	"slices"
	"sync"
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

// Members says whether a user currently belongs to a session.
type Members interface {
	IsMember(ctx context.Context, userID, sessionID string) (bool, error)
}

// SetMembers installs the membership check a user-level stream's deliveries need. Without one a
// user-level stream receives nothing (fail closed).
func (h *Hub) SetMembers(m Members) {
	h.mu.Lock()
	h.members = m
	h.mu.Unlock()
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

// noticeJob is one notice waiting for the notice worker. resolve, when set, builds the notice (a
// lookup that reads the store) on the worker; local means it came from another pod, so it is
// delivered here and goes no further.
type noticeJob struct {
	n       data.Notice
	resolve func(ctx context.Context) (data.Notice, bool)
	local   bool
}

const (
	noticeQueue   = 1024
	noticeTimeout = 3 * time.Second // bounds every store read one notice makes
)

// enqueueNotice hands j to the notice worker, started on first use. It never blocks: the
// preference, membership and manager reads a notice needs run on the worker under a timeout, so a
// slow store delays notices only, never a write, a request, or the fan-out tail's evictions. A full
// queue drops the notice (counted by Dropped, logged).
func (h *Hub) enqueueNotice(j noticeJob) {
	h.noticeOnce.Do(h.startNotices)
	h.noticePending.Add(1)
	select {
	case h.noticeQ <- j:
	default:
		h.noticePending.Done()
		h.dropped.Add(1)
		slog.Warn("the notice queue is full; dropping a notification", "type", j.n.Type, "session", j.n.SessionID)
	}
}

func (h *Hub) startNotices() {
	done := make(chan struct{})
	h.mu.Lock()
	h.noticeQ, h.noticeDone = make(chan noticeJob, noticeQueue), done
	h.mu.Unlock()
	h.noticeWG.Add(1)
	go func() {
		defer h.noticeWG.Done()
		for {
			select {
			case j := <-h.noticeQ:
				h.runNotice(j)
				h.noticePending.Done()
			case <-done:
				for {
					select {
					case <-h.noticeQ:
						h.noticePending.Done()
					default:
						return
					}
				}
			}
		}
	}()
}

func (h *Hub) runNotice(j noticeJob) {
	ctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "delivering a notification panicked", "panic", p)
		}
	}()
	n := j.n
	if j.resolve != nil {
		var ok bool
		if n, ok = j.resolve(ctx); !ok {
			return
		}
	}
	if n.SessionID == "" || len(n.Users) == 0 {
		return
	}
	h.notifyLocal(ctx, n)
	if j.local {
		return
	}
	if r := h.relay(); r != nil {
		r.publishNotice(n)
	}
}

// Flush waits until every notice queued so far has been delivered (tests, and shutdown).
func (h *Hub) Flush() { h.noticePending.Wait() }

func (h *Hub) stopNotices() {
	h.mu.Lock()
	started := h.noticeQ != nil
	done := h.noticeDone
	h.noticeDone = nil
	h.mu.Unlock()
	if started && done != nil {
		close(done)
		h.noticeWG.Wait()
	}
}

// Notify implements data.Notifier. It queues the notice; the worker delivers it to each
// addressee's open streams of the notice's session (a per-encounter stream only for that
// encounter), skipping an addressee who muted the type, and relays it to other pods. A stream's
// viewer is fixed when it is granted and ends when membership ends, so only current members of
// the session receive it. It never blocks the caller.
func (h *Hub) Notify(_ context.Context, n data.Notice) {
	if n.SessionID == "" || len(n.Users) == 0 {
		return
	}
	h.enqueueNotice(noticeJob{n: n})
}

// notifyLocal delivers n to this pod's streams only.
func (h *Hub) notifyLocal(ctx context.Context, n data.Notice) {
	ev := noticeEvent(n, newEventID())
	subs := h.snapshot(n.EncounterID)
	h.mu.RLock()
	prefs, members := h.prefs, h.members
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
		member := h.memberOnce(ctx, members, u, n.SessionID)
		for _, s := range subs {
			if s.addressedTo(u, n.SessionID, member) && s.send(ev) {
				s.overflowed.Store(true)
				h.dropped.Add(1)
				s.Close()
			}
		}
	}
}

// addressedTo reports whether s may receive a notice for user u in session: a session stream of
// u's in that session, or u's user-level stream when u is still a member (member is lazy).
func (s *Subscription) addressedTo(u, session string, member func() bool) bool {
	if s.userLevel {
		return s.actor == u && member()
	}
	v := s.viewer.Load()
	return v != nil && v.Actor() == u && v.SessionID() == session
}

// memberOnce returns a check of u's membership in session, read at most once per notice and only
// if a user-level stream needs it. A failed read counts as not a member.
func (h *Hub) memberOnce(ctx context.Context, m Members, u, session string) func() bool {
	var once sync.Once
	var ok bool
	return func() bool {
		once.Do(func() {
			if m == nil {
				return
			}
			var err error
			if ok, err = m.IsMember(ctx, u, session); err != nil {
				ok = false
				slog.WarnContext(ctx, "checking membership for a user-level notice failed; not delivering", "user", u, "error", err)
			}
		})
		return ok
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
func (h *Hub) ChangeSubmitted(_ context.Context, req data.ChangeRequest) {
	h.mu.RLock()
	m := h.managers
	h.mu.RUnlock()
	if m == nil {
		return
	}
	h.enqueueNotice(noticeJob{n: data.Notice{Type: data.NoticeChangeSubmitted, SessionID: req.SessionID}, resolve: func(ctx context.Context) (data.Notice, bool) {
		ids, err := m.ManagerIDs(ctx, req.SessionID)
		if err != nil {
			slog.WarnContext(ctx, "finding the session's managers failed; the change request notice was not sent", "session", req.SessionID, "error", err)
			return data.Notice{}, false
		}
		return requestNotice(data.NoticeChangeSubmitted, req, ids, req.CreatedAt), true
	}})
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
