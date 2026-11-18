package stream

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sweetrpg/initiative-api/data"
)

// Renderer gives the hub the tracker definition BuildView needs, looked up once per change.
// *domain.Encounters implements it.
type Renderer interface {
	Definition(ctx context.Context, enc data.Encounter) []data.TrackerField
}

// Bus receives the observer-level form of each event for the platform message bus. Publish must
// not block; the hub hands it events from a bounded queue and drops (and counts) when the queue is
// full, since bus publication is fail-open.
type Bus interface {
	Publish(ctx context.Context, e Event) error
}

// Hub is the data.Sink: it fans each committed change out to the subscriptions it concerns, after
// the commit, without blocking the writer. There is no outbox: a crash between commit and
// publication loses the event, which clients cover by refetching when they reconnect. A viewer who
// is not a GM or co-GM is told only of changes that alter their own view (see Event.ViewTag), so a
// hidden edit leaves them no gap.
type Hub struct {
	r   Renderer
	mu  sync.RWMutex
	sub map[*Subscription]struct{}
	bus *busQueue

	dropped atomic.Int64 // events not delivered: full subscriber buffer closed, or full bus queue
	emitted atomic.Int64
}

// New returns a hub that renders with r.
func New(r Renderer) *Hub { return &Hub{r: r, sub: map[*Subscription]struct{}{}} }

// Dropped is the number of events not delivered because a subscriber's buffer or the bus queue was
// full.
func (h *Hub) Dropped() int64 { return h.dropped.Load() }

// Emitted is the number of committed changes the hub has handled.
func (h *Hub) Emitted() int64 { return h.emitted.Load() }

// Subscription is one connection's feed. Its viewer proves what the subscriber may see; the
// subscriber must already have been authorized for it (data.Sessions.AuthorizeView or an owner
// viewer). With an encounter id it follows that encounter; with none it follows every encounter
// the viewer's audience hears of (a session's, or an owner's private ones), which is how
// encounter.created reaches anyone, since a new encounter has no subscribers of its own. The
// transport that carries it (5.2) decides which a connection asks for. C is closed when the
// subscription ends: by Close, by Evict, or by the hub when the subscriber falls behind.
type Subscription struct {
	C chan Event

	h           *Hub
	viewer      data.Viewer
	encounterID string
	overflowed  atomic.Bool

	mu     sync.Mutex // guards closed and every send on and close of C
	closed bool
}

// Subscribe registers a subscription with a buffer of buf events (at least 1). An empty
// encounterID follows every encounter in the viewer's audience.
func (h *Hub) Subscribe(v data.Viewer, encounterID string, buf int) *Subscription {
	s := &Subscription{C: make(chan Event, max(buf, 1)), h: h, viewer: v, encounterID: encounterID}
	h.mu.Lock()
	h.sub[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// send delivers ev without blocking. C is only ever sent on and closed under mu, so a send never
// meets a closed channel. overflow means the buffer was full.
func (s *Subscription) send(ev Event) (overflow bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.C <- ev:
		return false
	default:
		return true
	}
}

// Close ends the subscription. It is safe to call more than once and from any goroutine.
func (s *Subscription) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.C)
	}
	s.mu.Unlock()
	s.h.mu.Lock()
	delete(s.h.sub, s)
	s.h.mu.Unlock()
}

// Overflowed reports whether the hub closed the subscription because its buffer was full.
func (s *Subscription) Overflowed() bool { return s.overflowed.Load() }

// Evict closes every subscription of userID on the session, or of everyone on it when userID is
// empty. The subscription's viewer is fixed
// when it is made, so a user who is removed, blocked, leaves, or changes role must lose it: the
// client reconnects and is authorized again, at its new level or not at all.
func (h *Hub) Evict(sessionID, userID string) {
	h.mu.RLock()
	var gone []*Subscription
	for s := range h.sub {
		if s.viewer.SessionID() == sessionID && (userID == "" || s.viewer.Actor() == userID) {
			gone = append(gone, s)
		}
	}
	h.mu.RUnlock()
	for _, s := range gone {
		s.Close()
	}
}

// Emit implements data.Sink. It never blocks and never panics out: a failure to render or deliver
// for one subscriber affects only that subscriber.
func (h *Hub) Emit(ctx context.Context, c data.Change) {
	h.emitted.Add(1)
	em := newEmission(ctx, h.r, c)
	for _, s := range h.snapshot(c.After.ID) {
		h.deliver(ctx, em, s)
	}
	h.publishToBus(ctx, em)
}

// deliver sends the event s may receive for the change, recovering from a panic so one
// subscriber's failure cannot cost the others (or the bus) the event.
func (h *Hub) deliver(ctx context.Context, em *emission, s *Subscription) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "delivering an encounter event to a subscriber panicked", "panic", r)
		}
	}()
	if !inAudience(s.viewer, em.c) {
		return
	}
	ev, ok := em.forViewer(s.viewer)
	if !ok {
		return
	}
	if s.send(ev) {
		// A subscriber that cannot keep up is disconnected, not waited for: it reconnects and
		// refetches.
		s.overflowed.Store(true)
		h.dropped.Add(1)
		s.Close()
	}
}

func (h *Hub) snapshot(encounterID string) []*Subscription {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Subscription, 0, len(h.sub))
	for s := range h.sub {
		if s.encounterID == "" || s.encounterID == encounterID {
			out = append(out, s)
		}
	}
	return out
}

// inAudience reports whether v may hear of c at all. A session encounter's audience is its
// session's subscribers, taken from the session the encounter belonged to before the write (so a
// release or delete still reaches them); a private encounter's is its owner's.
func inAudience(v data.Viewer, c data.Change) bool {
	if v.Level() == data.ViewNone {
		return false
	}
	if c.SessionID != "" {
		return v.SessionID() == c.SessionID
	}
	return v.SessionID() == "" && v.Actor() == c.OwnerID
}

// ids are the ids a change reports, with duplicates removed.
func ids(c data.Change) []string {
	out := slices.Clone(c.CombatantID)
	slices.Sort(out)
	return slices.Compact(out)
}
