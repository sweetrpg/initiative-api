package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/events/stream"
)

func isClosed(s *stream.Subscription) bool {
	select {
	case _, ok := <-s.C:
		return !ok
	default:
		return false
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGrantRefusesAViewerThatCannotStream(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	gm := e.viewer(t, "user-ok")
	cases := map[string]data.Viewer{
		"the zero viewer":      {},
		"the bus viewer":       data.BusViewer(e.sid),
		"another user":         e.viewer(t, "user-two"),
		"another session only": data.BusViewer("other"),
	}
	for name, v := range cases {
		s := hub.Open(e.sid, "u-1", encE1, 4)
		if err := s.Grant(v); !errors.Is(err, stream.ErrViewer) {
			t.Errorf("Grant(%s) = %v, want ErrViewer", name, err)
		}
		s.Close()
	}
	s := hub.Open(e.sid, "u-1", encE1, 4)
	if err := s.Grant(gm); err != nil {
		t.Errorf("Grant(the matching viewer) = %v", err)
	}
}

func TestAPendingSubscriptionReceivesNothing(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	pending := hub.Open(e.sid, "u-1", encE1, 4)
	hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
	if n := len(drain(pending)); n != 0 {
		t.Errorf("a subscription not yet granted got %d events", n)
	}
}

func TestEvictedBetweenOpenAndGrantFailsTheGrant(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	player := e.viewer(t, "user-two")
	s := hub.Open(e.sid, "u-2", encE1, 4)
	hub.Evict(e.sid, "u-2") // the member is removed while authorization is in flight
	if err := s.Grant(player); !errors.Is(err, stream.ErrEvicted) {
		t.Errorf("Grant after eviction = %v, want ErrEvicted", err)
	}
	if !isClosed(s) {
		t.Error("an evicted pending subscription must be closed")
	}
}

func TestEvictWithoutASessionLeavesPrivateStreamsAlone(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	owner := subscribe(hub, ownerViewer(t, e), "", 4)
	hub.Evict("", "")
	hub.Evict("", "u-1")
	if isClosed(owner) {
		t.Error("Evict with no session id ended an owner's private stream")
	}
}

// relayPod is one API pod: a hub over the shared database, relaying through the shared collection.
type relayPod struct {
	hub   *stream.Hub
	relay *stream.Relay
}

func newRelayPods(t *testing.T, e eventTable, names ...string) []relayPod {
	t.Helper()
	var pods []relayPod
	for _, n := range names {
		h := stream.New(e.enc)
		r := h.AttachRelay(e.db.Collection(stream.FanoutCollection), n, 16)
		r.Run(context.Background())
		t.Cleanup(r.Stop)
		pods = append(pods, relayPod{h, r})
	}
	for _, p := range pods {
		waitFor(t, "the fan-out tail to open", p.relay.Tailing)
	}
	return pods
}

func (e eventTable) change(t *testing.T) data.Change {
	return data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()}
}

func TestAChangeOnOnePodReachesSubscribersOnTheOtherOnce(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	a, b := pods[0], pods[1]
	onA := subscribe(a.hub, e.viewer(t, "user-ok"), encE1, 8)
	onB := subscribe(b.hub, e.viewer(t, "user-ok"), encE1, 8)

	a.hub.Emit(context.Background(), e.change(t))

	waitFor(t, "the event on the other pod", func() bool { return len(onB.C) > 0 })
	time.Sleep(100 * time.Millisecond) // an echo to the originating pod would arrive by now
	if n := len(drain(onA)); n != 1 {
		t.Errorf("the originating pod delivered %d events, want 1 (its own rows are skipped)", n)
	}
	if n := len(drain(onB)); n != 1 {
		t.Errorf("the other pod delivered %d events to the GM, want 1", n)
	}
}

func TestARemoteChangeIsRenderedPerViewerOnTheReceivingPod(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	gm := subscribe(pods[1].hub, e.viewer(t, "user-ok"), encE1, 8)
	obs := subscribe(pods[1].hub, e.viewer(t, "user-three"), encE1, 8)

	pods[0].hub.Emit(context.Background(), e.change(t))

	waitFor(t, "the GM event", func() bool { return len(gm.C) > 0 })
	ev := <-gm.C
	if ev.Revision == 0 {
		t.Error("the GM's remote event lost its revision")
	}
	waitFor(t, "the observer event", func() bool { return len(obs.C) > 0 })
	if ev := <-obs.C; ev.Revision != 0 {
		t.Error("an observer's remote event carries a revision")
	}
}

func TestAnEvictionOnOnePodClosesTheSocketOnTheOther(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	player := subscribe(pods[1].hub, e.viewer(t, "user-two"), encE1, 8)
	gm := subscribe(pods[1].hub, e.viewer(t, "user-ok"), encE1, 8)
	pending := pods[1].hub.Open(e.sid, "u-2", encE1, 8)

	pods[0].hub.Evict(e.sid, "u-2")

	waitFor(t, "the remote eviction", func() bool { return isClosed(player) })
	if isClosed(gm) {
		t.Error("evicting one user ended another's stream")
	}
	if err := pending.Grant(e.viewer(t, "user-two")); !errors.Is(err, stream.ErrEvicted) {
		t.Errorf("a pending subscription on the other pod survived the eviction: %v", err)
	}
}

func TestAFailedTailDegradesToLocalDeliveryAndResumesWithoutLosingRows(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	a, b := pods[0], pods[1]
	onB := subscribe(b.hub, e.viewer(t, "user-ok"), encE1, 8)

	e.db.Fake(stream.FanoutCollection).BreakWatchers()
	waitFor(t, "pod B to report the failure", b.relay.Degraded)
	if b.relay.Failures() == 0 {
		t.Error("the failure was not counted")
	}

	b.hub.Emit(context.Background(), e.change(t)) // committed on B while its tail is down
	if n := len(drain(onB)); n != 1 {
		t.Errorf("a degraded pod delivered %d local events, want 1", n)
	}
	a.hub.Emit(context.Background(), e.change(t)) // written by A while B's tail is down

	waitFor(t, "the tail to resume", func() bool { return !b.relay.Degraded() && b.relay.Tailing() })
	waitFor(t, "the row written during the outage", func() bool { return len(onB.C) > 0 })
}
