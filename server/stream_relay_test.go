package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

func TestASecondGrantIsRefused(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	s := hub.Open(e.sid, "u-2", encE1, 4)
	if err := s.Grant(e.viewer(t, "user-two")); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(e.viewer(t, "user-two")); !errors.Is(err, stream.ErrGranted) {
		t.Errorf("second Grant = %v, want ErrGranted", err)
	}
}

func TestAPendingSubscriptionIsClosedAfterItsDeadline(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	hub.SetPendingTTL(30 * time.Millisecond)
	pending := hub.Open(e.sid, "u-2", encE1, 4)
	granted := hub.Open(e.sid, "u-3", encE1, 4)
	if err := granted.Grant(e.viewer(t, "user-three")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pending subscription to be closed", func() bool { return isClosed(pending) })
	if isClosed(granted) {
		t.Error("a granted subscription was closed by the pending deadline")
	}
	if err := pending.Grant(e.viewer(t, "user-two")); !errors.Is(err, stream.ErrEvicted) {
		t.Errorf("Grant after the deadline = %v, want ErrEvicted", err)
	}
}

func TestAnEvictionRowWithNoSessionEndsNothing(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-b")
	owner := subscribe(pods[0].hub, ownerViewer(t, e), "", 4)
	row := map[string]any{"_id": primitive.NewObjectID(), "pod": "pod-x", "kind": "evict", "at": time.Now(), "user_id": "u-1"}
	if err := e.db.Collection(stream.FanoutCollection).InsertOne(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if isClosed(owner) {
		t.Error("an eviction row with no session ended a private stream")
	}
}

func TestAnEvictionWrittenDuringATransientOutageStillArrives(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	player := subscribe(pods[1].hub, e.viewer(t, "user-two"), encE1, 8)
	gm := subscribe(pods[1].hub, e.viewer(t, "user-ok"), encE1, 8)

	e.db.Fake(stream.FanoutCollection).BreakWatchers()
	waitFor(t, "pod B to notice", pods[1].relay.Degraded)
	pods[0].hub.Evict(e.sid, "u-2") // written while B's tail is down

	waitFor(t, "the eviction written in the gap", func() bool { return isClosed(player) })
	if isClosed(gm) {
		t.Error("a transient outage ended an unrelated stream")
	}
	if n := pods[1].relay.GapEvictions(); n != 0 {
		t.Errorf("a resumable outage ended every stream (%d gap evictions)", n)
	}
}

func TestALostHistoryEndsEveryStreamBecauseEvictionsInTheGapAreUnknown(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-b") // one pod: the fake loses history for the first resume only
	gm := subscribe(pods[0].hub, e.viewer(t, "user-ok"), encE1, 8)
	player := subscribe(pods[0].hub, e.viewer(t, "user-two"), encE1, 8)

	e.db.Fake(stream.FanoutCollection).LoseHistory()
	e.db.Fake(stream.FanoutCollection).BreakWatchers()

	waitFor(t, "every stream to end after the gap", func() bool { return isClosed(gm) && isClosed(player) })
	if n := pods[0].relay.GapEvictions(); n != 1 {
		t.Errorf("gap evictions = %d, want 1", n)
	}
	waitFor(t, "the tail to reopen", pods[0].relay.Tailing)
}

// slowCollection delays every insert, as a Mongo slowdown does.
type slowCollection struct {
	store.Collection
	delay time.Duration
}

func (c slowCollection) Watch(ctx context.Context, resume bson.Raw) (store.Cursor, error) {
	return store.Watch(ctx, c.Collection, resume)
}

func (c slowCollection) InsertOne(ctx context.Context, doc any) error {
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return ctx.Err() // a real driver abandons the insert when its context ends
	}
	return c.Collection.InsertOne(ctx, doc)
}

func TestStopWritesAnEvictionStillBeingInserted(t *testing.T) {
	e := newEventTable(t, nil)
	h := stream.New(e.enc)
	r := h.AttachRelay(slowCollection{e.db.Collection(stream.FanoutCollection), 300 * time.Millisecond}, "pod-a", 4)
	r.Run(context.Background())
	waitFor(t, "the tail", r.Tailing)

	h.Evict(e.sid, "u-2")
	time.Sleep(50 * time.Millisecond) // the writer has taken the row and is mid-insert
	r.Stop()

	if n := len(e.db.Fake(stream.FanoutCollection).Docs()); n != 1 {
		t.Errorf("the fan-out collection holds %d rows after Stop, want the eviction", n)
	}
}

func TestStopWithoutRunReturnsAtOnce(t *testing.T) {
	e := newEventTable(t, nil)
	r := stream.New(e.enc).AttachRelay(e.db.Collection(stream.FanoutCollection), "pod-a", 4)
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waited on a relay that never ran")
	}
}

func TestAGrantThatSucceedsIsNeverFollowedByThePendingClose(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	hub.SetPendingTTL(time.Millisecond)
	v := e.viewer(t, "user-two")
	for range 300 {
		s := hub.Open(e.sid, "u-2", encE1, 4)
		time.Sleep(time.Millisecond) // land the deadline close to the grant
		if err := s.Grant(v); err != nil {
			continue
		}
		time.Sleep(5 * time.Millisecond)
		if isClosed(s) {
			t.Fatal("a subscription whose Grant succeeded was closed by the pending deadline")
		}
		s.Close()
	}
}

type slowRenderer struct{ stream.Renderer }

func (r slowRenderer) Definition(ctx context.Context, enc data.Encounter) []data.TrackerField {
	time.Sleep(100 * time.Millisecond)
	return r.Renderer.Definition(ctx, enc)
}

func TestAFullRemoteQueueDisconnectsTheAudienceInsteadOfLeavingItStale(t *testing.T) {
	e := newEventTable(t, nil)
	a := stream.New(e.enc)
	ra := a.AttachRelay(e.db.Collection(stream.FanoutCollection), "pod-a", 64)
	b := stream.New(slowRenderer{e.enc})
	rb := b.AttachRelay(e.db.Collection(stream.FanoutCollection), "pod-b", 1)
	for _, r := range []*stream.Relay{ra, rb} {
		r.Run(context.Background())
		t.Cleanup(r.Stop)
		waitFor(t, "the tail", r.Tailing)
	}
	player := subscribe(b, e.viewer(t, "user-two"), encE1, 64)

	for range 20 {
		a.Emit(context.Background(), e.change(t))
	}

	waitFor(t, "the audience to be disconnected", func() bool { return isClosed(player) })
	if rb.Dropped() == 0 {
		t.Error("the dropped remote rows were not counted")
	}
}

func TestAnAddressedNoticeOnOnePodReachesTheAddresseeOnTheOtherOnce(t *testing.T) {
	e := newEventTable(t, nil)
	pods := newRelayPods(t, e, "pod-a", "pod-b")
	onA := subscribe(pods[0].hub, e.viewer(t, "user-two"), encE1, 8)
	onB := subscribe(pods[1].hub, e.viewer(t, "user-two"), encE1, 8)
	other := subscribe(pods[1].hub, e.viewer(t, "user-three"), encE1, 8)

	pods[0].hub.Notify(context.Background(), data.Notice{Type: data.NoticeTurnYours, SessionID: e.sid, EncounterID: encE1, Users: []string{"u-2"}, At: time.Now(), Data: map[string]any{"combatant_id": "pc"}})

	waitFor(t, "the notice on the other pod", func() bool { return len(onB.C) > 0 })
	if n := len(drain(onA)); n != 1 {
		t.Errorf("the originating pod delivered %d, want 1", n)
	}
	if got := drain(onB); len(got) != 1 || got[0].Type != data.NoticeTurnYours {
		t.Errorf("the other pod delivered %+v, want one turn.yours", got)
	}
	if n := len(drain(other)); n != 0 {
		t.Errorf("a different user on the other pod received %d notices", n)
	}
}
