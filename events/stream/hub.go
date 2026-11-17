package stream

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

// Renderer renders a stored encounter for one viewer through the view builder.
// *domain.Encounters implements it.
type Renderer interface {
	Render(ctx context.Context, enc data.Encounter, v data.Viewer) (domain.EncounterView, error)
}

// Bus receives the player-level form of each event for the platform message bus. Publish must not
// block; the hub hands it events from a bounded queue and drops (and counts) when the queue is
// full, since bus publication is fail-open.
type Bus interface {
	Publish(ctx context.Context, e Event) error
}

// Hub is the data.Sink: it fans each committed change out to the subscriptions it concerns, after
// the commit, without blocking the writer. There is no outbox: a crash between commit and
// publication loses the event, which clients cover by refetching when they reconnect or see a
// gap. Hidden-combatant changes advance the revision too, so a player sees gaps they cannot
// explain; the event for them carries no id.
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

// Subscription is one connection's feed of one encounter. Its viewer proves what the subscriber may
// see; the subscriber must already have been authorized for it (data.Sessions.AuthorizeView or an
// owner viewer). C is closed when the subscription ends: by Close, or by the hub when the
// subscriber falls behind and its buffer fills.
type Subscription struct {
	C chan Event

	h           *Hub
	viewer      data.Viewer
	encounterID string
	once        sync.Once
	overflowed  atomic.Bool
}

// Subscribe registers a subscription for encounterID with a buffer of buf events (at least 1).
func (h *Hub) Subscribe(v data.Viewer, encounterID string, buf int) *Subscription {
	s := &Subscription{C: make(chan Event, max(buf, 1)), h: h, viewer: v, encounterID: encounterID}
	h.mu.Lock()
	h.sub[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.h.mu.Lock()
		delete(s.h.sub, s)
		s.h.mu.Unlock()
		close(s.C)
	})
}

// Overflowed reports whether the hub closed the subscription because its buffer was full.
func (s *Subscription) Overflowed() bool { return s.overflowed.Load() }

// Emit implements data.Sink. It never blocks and never panics out: a failure to render or deliver
// for one subscriber affects only that subscriber.
func (h *Hub) Emit(ctx context.Context, c data.Change) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "emitting an encounter event panicked", "panic", r)
		}
	}()
	h.emitted.Add(1)
	em := newEmission(ctx, h.r, c)
	for _, s := range h.snapshot(c.After.ID) {
		if !inAudience(s.viewer, c) {
			continue
		}
		ev, ok := em.forViewer(s.viewer)
		if !ok {
			continue
		}
		select {
		case s.C <- ev:
		default:
			// A subscriber that cannot keep up is disconnected, not waited for: it reconnects and
			// is told the current revision first.
			s.overflowed.Store(true)
			h.dropped.Add(1)
			s.Close()
		}
	}
	h.publishToBus(ctx, em)
}

func (h *Hub) snapshot(encounterID string) []*Subscription {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Subscription, 0, len(h.sub))
	for s := range h.sub {
		if s.encounterID == encounterID {
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
