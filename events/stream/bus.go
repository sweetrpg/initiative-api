package stream

import (
	"context"
	"log/slog"
	"sync"
	"time"
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
