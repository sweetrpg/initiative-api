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
// queue, or a hub that has been closed, drops the notice (counted by Dropped, logged). The send
// happens under the read lock, so Close (which takes the write lock) never races a send that would
// then have no consumer.
func (h *Hub) enqueueNotice(j noticeJob) {
	h.noticeOnce.Do(h.startNotices)
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.noticeClosed || h.noticeQ == nil {
		h.dropNotice(j, "the hub is closed")
		return
	}
	h.noticePending.Add(1)
	select {
	case h.noticeQ <- j:
	default:
		h.noticePending.Done()
		h.dropNotice(j, "the notice queue is full")
	}
}

func (h *Hub) dropNotice(j noticeJob, why string) {
	h.dropped.Add(1)
	slog.Warn("dropping a notification: "+why, "type", j.n.Type, "session", j.n.SessionID)
}

// startNotices starts the worker unless the hub has already been closed, which is final for
// notices.
func (h *Hub) startNotices() {
	h.mu.Lock()
	if h.noticeClosed {
		h.mu.Unlock()
		return
	}
	done := make(chan struct{})
	q := make(chan noticeJob, noticeQueue)
	h.noticeQ, h.noticeDone = q, done
	h.mu.Unlock()
	h.noticeWG.Add(1)
	go func() {
		defer h.noticeWG.Done()
		for {
			select {
			case j := <-q:
				h.runNotice(j)
				h.noticePending.Done()
			case <-done:
				for {
					select {
					case j := <-q: // queued but never delivered: counted, not silent
						h.dropNotice(j, "the hub closed before it was delivered")
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
	muted := h.mutedUsers(n) // one read per addressee, shared by every delivery of this notice
	h.notifyLocal(n, muted)
	if j.local {
		return
	}
	if r := h.relay(); r != nil {
		r.publishNotice(n)
	}
	h.publishNoticeToBus(n, muted)
}

// Flush waits until every notice queued so far has been delivered (tests, and shutdown).
func (h *Hub) Flush() { h.noticePending.Wait() }

// stopNotices ends the notice worker for good: later notices are dropped and counted, and
// Flush returns.
func (h *Hub) stopNotices() {
	h.mu.Lock()
	h.noticeClosed = true
	done := h.noticeDone
	h.noticeDone = nil
	h.mu.Unlock()
	if done != nil {
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

// notifyLocal delivers n to this pod's streams only, skipping the addressees in muted.
func (h *Hub) notifyLocal(n data.Notice, muted map[string]bool) {
	ev := noticeEvent(n, newEventID())
	subs := h.snapshot(n.EncounterID)
	h.mu.RLock()
	members := h.members
	h.mu.RUnlock()
	done := map[string]bool{}
	for _, u := range n.Users {
		if u == "" || done[u] || muted[u] {
			continue
		}
		done[u] = true
		member := h.memberOnce(members, u, n.SessionID)
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
func (h *Hub) memberOnce(m Members, u, session string) func() bool {
	var once sync.Once
	var ok bool
	return func() bool {
		once.Do(func() {
			if m == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), muteLookupTimeout)
			defer cancel()
			var err error
			if ok, err = m.IsMember(ctx, u, session); err != nil {
				ok = false
				slog.WarnContext(ctx, "checking membership for a user-level notice failed; not delivering", "user", u, "error", err)
			}
		})
		return ok
	}
}

// muteLookupTimeout bounds each addressee's preference read.
const muteLookupTimeout = time.Second

// mutedUsers returns the addressees who turned n's type off for its session, with one preference
// read per distinct addressee, each under its own timeout.
//
// A failed read fails open on purpose: a notification is worth more than a missed mute, and a mute
// only chooses what a client shows. It is never silent: the first failure is logged at WARN with
// how many addressees were left unchecked, and the remaining reads are skipped, so a dead store
// costs one timeout per notice, not one per addressee.
func (h *Hub) mutedUsers(n data.Notice) map[string]bool {
	h.mu.RLock()
	prefs := h.prefs
	h.mu.RUnlock()
	if prefs == nil {
		return nil
	}
	muted := map[string]bool{}
	seen := map[string]bool{}
	for i, u := range n.Users {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		ctx, cancel := context.WithTimeout(context.Background(), muteLookupTimeout)
		list, err := prefs.Muted(ctx, u, n.SessionID)
		cancel()
		if err != nil {
			slog.Warn("reading notification preferences failed; delivering without the mutes of the rest",
				"type", n.Type, "session", n.SessionID, "unchecked", len(n.Users)-i, "error", err)
			break
		}
		if slices.Contains(list, n.Type) {
			muted[u] = true
		}
	}
	return muted
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
