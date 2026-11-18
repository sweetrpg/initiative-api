package stream

import (
	"context"
	"errors"
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
	rel *Relay

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

// Subscription is one connection's feed. It is made pending by Open, receives nothing until Grant
// gives it the viewer proving what the subscriber may see, and ends by Close, by Evict, or when the
// hub finds the subscriber behind. With an encounter id it follows that encounter; with none it
// follows every encounter the viewer's audience hears of (a session's, or an owner's private
// ones), which is how encounter.created reaches anyone, since a new encounter has no subscribers
// of its own. C is closed when the subscription ends.
type Subscription struct {
	C chan Event

	h           *Hub
	session     string // fixed by Open; the audience Evict matches, before and after Grant
	actor       string
	encounterID string
	viewer      atomic.Pointer[data.Viewer] // nil until Grant
	overflowed  atomic.Bool

	mu     sync.Mutex // guards closed, granted, and every send on and close of C
	closed bool
}

// ErrEvicted means the subscription ended (evicted, closed, or overflowed) before Grant.
var ErrEvicted = errors.New("stream: subscription ended before it was granted")

// ErrViewer means Grant was given a viewer that cannot stream: a zero one, one with no actor (such
// as BusViewer), or one for a different actor or session than Open named.
var ErrViewer = errors.New("stream: viewer does not match the subscription")

// Open registers a pending subscription of actor in the session ("" for an owner's private
// encounters) with a buffer of buf events (at least 1). An empty encounterID follows every
// encounter in the viewer's audience. A pending subscription receives nothing, but Evict already
// reaches it, so an eviction that lands while the subscriber is being authorized is not lost:
// authorize, then Grant. Close it if authorization fails.
func (h *Hub) Open(sessionID, actor, encounterID string, buf int) *Subscription {
	s := &Subscription{C: make(chan Event, max(buf, 1)), h: h, session: sessionID, actor: actor, encounterID: encounterID}
	h.mu.Lock()
	h.sub[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Grant makes the subscription live for v, which the caller obtained by authorizing the actor
// (data.Sessions.AuthorizeView or an owner viewer). It fails with ErrEvicted if the subscription
// ended since Open, and with ErrViewer if v could not stream.
func (s *Subscription) Grant(v data.Viewer) error {
	if v.Level() == data.ViewNone || v.Actor() == "" || v.Actor() != s.actor || v.SessionID() != s.session {
		return ErrViewer
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrEvicted
	}
	s.viewer.Store(&v)
	return nil
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

// Evict closes every subscription (pending or live) of userID on the session, or of everyone on it
// when userID is empty, here and on the other pods (see Relay). A subscription's viewer is fixed
// when it is granted, so a user who is removed, blocked, leaves, or changes role must lose it: the
// client reconnects and is authorized again, at its new level or not at all.
func (h *Hub) Evict(sessionID, userID string) {
	if sessionID == "" {
		return // an empty session would match every private subscription
	}
	h.evictLocal(sessionID, userID)
	if r := h.relay(); r != nil {
		r.publishEvict(sessionID, userID)
	}
}

func (h *Hub) evictLocal(sessionID, userID string) {
	h.mu.RLock()
	var gone []*Subscription
	for s := range h.sub {
		if s.session == sessionID && (userID == "" || s.actor == userID) {
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
	h.deliverLocal(ctx, em)
	h.publishToBus(ctx, em)
	if r := h.relay(); r != nil {
		r.publishChange(c)
	}
}

// deliverLocal sends the change to this pod's subscribers.
func (h *Hub) deliverLocal(ctx context.Context, em *emission) {
	for _, s := range h.snapshot(em.c.After.ID) {
		h.deliver(ctx, em, s)
	}
}

// emitRemote delivers a change another pod committed to this pod's subscribers. The bus and the
// relay are the originating pod's to feed, so neither runs here.
func (h *Hub) emitRemote(ctx context.Context, c data.Change) {
	h.emitted.Add(1)
	h.deliverLocal(ctx, newEmission(ctx, h.r, c))
}

// deliver sends the event s may receive for the change, recovering from a panic so one
// subscriber's failure cannot cost the others (or the bus) the event.
func (h *Hub) deliver(ctx context.Context, em *emission, s *Subscription) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "delivering an encounter event to a subscriber panicked", "panic", r)
		}
	}()
	v := s.viewer.Load()
	if v == nil {
		return // pending
	}
	if !inAudience(*v, em.c) {
		return
	}
	ev, ok := em.forViewer(*v)
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
