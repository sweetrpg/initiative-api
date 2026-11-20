package stream

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

// busPublishTimeout bounds one publication, so a stuck broker cannot hold the queue.
const busPublishTimeout = 2 * time.Second

type busQueue struct {
	bus  Bus
	ch   chan Event
	done chan struct{}
	wg   sync.WaitGroup
}

// AttachBus publishes the bus form of every event to bus from a queue of the given size, in a
// goroutine, fail-open: a full queue or a failed publication drops the event (counted by Dropped
// and logged) and never affects the write or the WebSocket subscribers. Call Close to stop it.
func (h *Hub) AttachBus(bus Bus, queue int) {
	h.closeBus() // a second attach replaces the first, whose goroutine must not outlive it
	q := &busQueue{bus: bus, ch: make(chan Event, max(queue, 1)), done: make(chan struct{})}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			select {
			case ev := <-q.ch:
				ctx, cancel := context.WithTimeout(context.Background(), busPublishTimeout)
				if err := bus.Publish(ctx, ev); err != nil {
					h.dropped.Add(1)
					slog.Warn("publishing an encounter event to the bus failed", "event_id", ev.EventID, "type", ev.Type, "error", err)
				}
				cancel()
			case <-q.done:
				return
			}
		}
	}()
	h.mu.Lock()
	h.bus = q
	h.mu.Unlock()
}

// Close stops the bus and notice goroutines, if started.
func (h *Hub) Close() {
	h.closeBus()
	h.stopNotices()
}

func (h *Hub) closeBus() {
	h.mu.Lock()
	q := h.bus
	h.bus = nil
	h.mu.Unlock()
	if q != nil {
		close(q.done)
		q.wg.Wait()
	}
}

// publishNoticeToBus queues one event per addressee for the bus, where a push service can consume
// it. It carries what the addressee's own stream carries and is published on the addressee's own
// subject. Two addressees are left out: one who muted the type (the consumer has no other way to
// see the mute) and one who is no longer a member of the session, since nothing ends a push the way
// a stream's eviction ends a stream (so a removed requester gets no change_request.approved).
// Fail-open like every bus publication, except the membership check, which fails closed.
func (h *Hub) publishNoticeToBus(n data.Notice, muted map[string]bool) {
	h.mu.RLock()
	q, members := h.bus, h.members
	h.mu.RUnlock()
	if q == nil {
		return
	}
	seen := map[string]bool{}
	for _, u := range n.Users {
		if u == "" || seen[u] || muted[u] {
			continue
		}
		seen[u] = true
		if !h.memberOnce(members, u, n.SessionID)() {
			continue
		}
		ev := noticeEvent(n, newEventID())
		ev.UserID = u
		select {
		case q.ch <- ev:
		default:
			h.dropped.Add(1)
			slog.Warn("the bus queue is full; dropping a notice", "event_id", ev.EventID, "type", ev.Type)
		}
	}
}

func (h *Hub) publishToBus(ctx context.Context, em *emission) {
	h.mu.RLock()
	q := h.bus
	h.mu.RUnlock()
	// A private encounter has no audience beyond its owner; the bus is for what a session's other
	// consumers may react to.
	if q == nil || (em.c.SessionID == "" && em.c.After.SessionID == "") {
		return
	}
	ev, ok := em.busEvent()
	if !ok {
		return
	}
	select {
	case q.ch <- ev:
	default:
		h.dropped.Add(1)
		slog.WarnContext(ctx, "the bus queue is full; dropping an encounter event", "event_id", ev.EventID, "type", ev.Type)
	}
}
