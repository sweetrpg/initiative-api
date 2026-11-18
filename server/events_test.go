package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// recordSink records every Change the data layer reports, and forwards it to next when set.
type recordSink struct {
	mu      sync.Mutex
	changes []data.Change
	next    data.Sink
}

func (s *recordSink) Emit(ctx context.Context, c data.Change) {
	s.mu.Lock()
	s.changes = append(s.changes, c)
	s.mu.Unlock()
	if s.next != nil {
		s.next.Emit(ctx, c)
	}
}

func (s *recordSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.changes)
}

func (s *recordSink) last() data.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes[len(s.changes)-1]
}

// eventTable is the viewTable session with a sink installed on every encounter repository, and a
// way to obtain the viewer of each fixture user.
type eventTable struct {
	r    http.Handler
	db   *fake.DB
	sid  string
	sink *recordSink
	enc  *domain.Encounters
	ses  *domain.Sessions
	rel  *httptest.Server
}

func newEventTable(t *testing.T, next func(*domain.Encounters) data.Sink) eventTable {
	t.Helper()
	return newEventTableLimit(t, "3", next)
}

func newEventTableLimit(t *testing.T, limit string, next func(*domain.Encounters) data.Sink) eventTable {
	t.Helper()
	e := eventTable{sink: &recordSink{}}
	routerHook = func(svc *Services) {
		e.enc, e.ses = svc.Encounters, svc.Sessions
		if next != nil {
			e.sink.next = next(svc.Encounters)
		}
		svc.Encounters.SetSink(e.sink)
		svc.Sessions.SetSink(e.sink)
		svc.Rosters.SetSink(e.sink)
	}
	t.Cleanup(func() { routerHook = nil })
	e.r, e.db, e.sid = viewTableLimit(t, limit)
	seedSystem(t, e.db)
	e.rel = httptest.NewServer(http.HandlerFunc(roleChecks))
	t.Cleanup(e.rel.Close)
	return e
}

// viewer is the session Viewer of the fixture user behind token (user-ok is the GM u-1, user-two
// the player u-2 who controls the pc, user-three the observer u-3).
func (e eventTable) viewer(t *testing.T, token string) data.Viewer {
	t.Helper()
	actor := map[string]string{"user-ok": "u-1", "user-two": "u-2", "user-three": "u-3"}[token]
	ctx := data.WithGate(context.Background(), relations.New(e.rel.URL), token)
	v, err := e.ses.Viewer(ctx, actor, e.sid)
	if err != nil {
		t.Fatalf("viewer for %s: %v", token, err)
	}
	return v
}

func (e eventTable) encounter(t *testing.T) data.Encounter {
	t.Helper()
	var enc data.Encounter
	if err := e.db.Fake(data.CollectionEncounters).FindOne(context.Background(), bson.D{{Key: "_id", Value: encE1}}, &enc); err != nil {
		t.Fatal(err)
	}
	return enc
}

func drain(s *stream.Subscription) []stream.Event {
	var out []stream.Event
	for {
		select {
		case ev, ok := <-s.C:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

var allEventTypes = []string{
	data.EventCreated, data.EventUpdated, data.EventDeleted, data.EventReleased, data.EventStarted, data.EventEnded, data.EventReset,
	data.EventTurn, data.EventCombatantAdded, data.EventCombatantUpdated, data.EventCombatantRemoved, data.EventCombatantRestored, data.EventControllers,
}

func TestNoEventAPlayerOrObserverCanReceiveCarriesAHiddenCombatant(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	enc := e.encounter(t) // the hidden adversary "hid" is the current turn
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 64)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 64)
	observer := hub.Subscribe(e.viewer(t, "user-three"), encE1, 64)
	secrets := []string{`"hid"`, secretName, secretNote, `"initiative":`, `"health"`, `:31`, `"revision"`}

	for _, typ := range allEventTypes {
		for _, ids := range [][]string{{"hid"}, {"hid", "orc"}, {"orc"}, nil} {
			hub.Emit(context.Background(), data.Change{Type: typ, After: enc, SessionID: e.sid, OwnerID: "u-1", CombatantID: ids, At: time.Now()})
		}
	}

	for name, sub := range map[string]*stream.Subscription{"player": player, "observer": observer} {
		evs := drain(sub)
		if len(evs) != len(allEventTypes)*4 {
			t.Fatalf("%s received %d events, want one per change", name, len(evs))
		}
		for _, ev := range evs {
			body := js(ev)
			for _, secret := range secrets {
				if strings.Contains(body, secret) {
					t.Errorf("%s got %s carrying %s: %s", name, ev.Type, secret, body)
				}
			}
		}
	}
	// The GM is told everything, hidden ids included, and orders by revision.
	for _, ev := range drain(gm) {
		if ev.Revision < 1 || ev.ViewTag != "" {
			t.Errorf("the GM's %s has revision %d and view_tag %q, want a revision and no view_tag", ev.Type, ev.Revision, ev.ViewTag)
		}
	}
}

func TestAPlayerSeesTheVisibleIdsOfAChangeThatAlsoTouchedAHiddenOne(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid", "orc"}, At: time.Now()})
	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid"}, At: time.Now()})
	hub.Emit(context.Background(), data.Change{Type: data.EventTurn, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
	got := drain(player)

	if len(got) != 3 {
		t.Fatalf("%d events", len(got))
	}
	if ids, _ := got[0].Data["combatant_ids"].([]string); got[0].Type != data.EventCombatantUpdated || len(ids) != 1 || ids[0] != "orc" {
		t.Errorf("mixed change = %s %v, want combatant.updated for orc only", got[0].Type, got[0].Data)
	}
	if got[1].Type != data.EventUpdated || len(got[1].Data) != 0 {
		t.Errorf("hidden-only change = %s %v, want a generic encounter.updated with no ids", got[1].Type, got[1].Data)
	}
	if _, has := got[2].Data["current_combatant_id"]; has || got[2].Type != data.EventTurn {
		t.Errorf("turn.advanced with a hidden current combatant = %v, want the current id left out", got[2].Data)
	}
}

func TestAGMOnlyChangeReachesOnlyTheGMAndNeverTheBus(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	bus := &captureBus{}
	hub.AttachBus(bus, 8)
	defer hub.Close()
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
	observer := hub.Subscribe(e.viewer(t, "user-three"), encE1, 8)

	hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, GMOnly: true, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})

	g := drain(gm)
	if len(g) != 1 || g[0].Type != data.EventUpdated || len(g[0].Data) != 0 {
		t.Errorf("GM got %v, want one encounter.updated with no data", g)
	}
	if len(drain(player)) != 0 || len(drain(observer)) != 0 {
		t.Error("a lock or inactive change reached a player or observer")
	}
	time.Sleep(50 * time.Millisecond) // nothing to poll for: the assertion is that nothing arrives
	if bus.count() != 0 {
		t.Error("a GM-only change was published to the bus")
	}
}

type captureBus struct {
	mu     sync.Mutex
	events []stream.Event
	err    error
	block  chan struct{}
}

func (b *captureBus) Publish(ctx context.Context, ev stream.Event) error {
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return b.err
}

func (b *captureBus) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.events) }

func TestAnEventFailsClosedWhenAViewerCannotBeRendered(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)
	// A document the view builder refuses for the player's session: BuildView returns ErrNoView.
	odd := e.encounter(t)
	odd.SessionID = "5f00000000000000000000bb"

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUpdated, After: odd, SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid", "orc"}, At: time.Now()})

	p := drain(player)
	if len(p) != 1 || p[0].Type != data.EventUpdated || len(p[0].Data) != 0 || strings.Contains(js(p), "orc") {
		t.Fatalf("player got %v, want the generic encounter.updated with no ids, never the unfiltered event", p)
	}
	if g := drain(gm); len(g) != 1 || g[0].Type != data.EventCombatantUpdated {
		t.Errorf("the GM, who needs no rendering, got %v", g)
	}
}

type panickingRenderer struct{}

func (panickingRenderer) Definition(context.Context, data.Encounter) []data.TrackerField {
	panic("definition lookup blew up")
}

func TestOneSubscribersFailureDoesNotCostTheOthersTheEvent(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(panickingRenderer{})
	bus := &captureBus{}
	hub.AttachBus(bus, 8)
	defer hub.Close()
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8) // needs a rendering, which panics
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)      // needs none

	hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})

	if len(drain(gm)) != 1 {
		t.Error("the GM lost the event because another subscriber's delivery panicked")
	}
	if len(drain(player)) != 0 {
		t.Error("the player whose delivery panicked still got something")
	}
	eventually(t, func() bool { return bus.count() == 1 }, "the bus lost the event too")
}

type panickingSink struct{}

func (panickingSink) Emit(context.Context, data.Change) { panic("sink blew up") }

func TestASinkThatPanicsNeverFailsAWriteThatCommitted(t *testing.T) {
	e := newEventTableLimit(t, "50", func(*domain.Encounters) data.Sink { return panickingSink{} })

	w := callAs(e.r, "user-ok", "POST", "/encounters", `{"name":"N","game_system_id":"`+data.DefaultSystemID+`"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("create with a panicking sink = %d %s, want the committed write to succeed", w.Code, w.Body)
	}
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestTheClientIDReachesOnlyTheOriginatingUsersOwnStreams(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	bus := &captureBus{}
	hub.AttachBus(bus, 8)
	defer hub.Close()
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
	origin := data.Origin{Kind: data.OriginUser, ClientID: "ios-7.f", ActorID: "u-1"}

	hub.Emit(context.Background(), data.Change{Type: data.EventTurn, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", Origin: origin, At: time.Now()})
	eventually(t, func() bool { return bus.count() == 1 }, "the bus never got the event")

	g, p := drain(gm), drain(player)
	if g[0].Origin.Kind != "user" || g[0].Origin.ClientID != "ios-7.f" {
		t.Errorf("the originating user's stream = %+v, want their own client id", g[0].Origin)
	}
	if p[0].Origin.Kind != "user" || p[0].Origin.ClientID != "" {
		t.Errorf("another user's stream = %+v, want the kind and no client id", p[0].Origin)
	}
	if bus.count() != 1 || bus.events[0].Origin.ClientID != "" || bus.events[0].Revision != 0 {
		t.Errorf("the bus form carries a client id or a revision: %v", bus.events)
	}
}

func TestASubscriberThatFallsBehindIsClosedAndOthersAreUnaffected(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	slow := hub.Subscribe(e.viewer(t, "user-two"), encE1, 1)
	fast := hub.Subscribe(e.viewer(t, "user-three"), encE1, 8)
	other := hub.Subscribe(e.viewer(t, "user-three"), "5f00000000000000000000e2", 8) // another encounter

	for i := 0; i < 3; i++ {
		hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
	}

	if !slow.Overflowed() || hub.Dropped() != 1 {
		t.Errorf("slow overflowed = %v, dropped = %d; want the full subscriber closed and counted once", slow.Overflowed(), hub.Dropped())
	}
	if n := len(drain(fast)); n != 3 {
		t.Errorf("the other subscriber got %d events, want 3", n)
	}
	if len(drain(other)) != 0 {
		t.Error("a subscription to another encounter received an event")
	}
}

func TestABusThatBlocksOrFailsNeverHoldsUpTheWriterOrTheStream(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	bus := &captureBus{block: make(chan struct{}), err: errors.New("broker down")}
	hub.AttachBus(bus, 1)
	defer hub.Close()
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 16)

	start := time.Now()
	for i := 0; i < 10; i++ {
		hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
	}

	if time.Since(start) > time.Second {
		t.Fatalf("10 emits with a blocked bus took %v", time.Since(start))
	}
	if n := len(drain(player)); n != 10 {
		t.Errorf("the stream delivered %d of 10 events while the bus was blocked", n)
	}
	if hub.Dropped() < 8 {
		t.Errorf("dropped = %d, want the overflowing bus publications counted", hub.Dropped())
	}
	close(bus.block)
}

type countingRenderer struct {
	inner stream.Renderer
	n     atomic.Int64
}

func (c *countingRenderer) Definition(ctx context.Context, enc data.Encounter) []data.TrackerField {
	c.n.Add(1)
	return c.inner.Definition(ctx, enc)
}

func TestADefinitionIsLookedUpOncePerEmissionNotOncePerSubscriber(t *testing.T) {
	e := newEventTable(t, nil)
	cr := &countingRenderer{inner: e.enc}
	hub := stream.New(cr)
	for i := 0; i < 5; i++ {
		hub.Subscribe(e.viewer(t, "user-ok"), encE1, 4)    // GM: needs no rendering
		hub.Subscribe(e.viewer(t, "user-three"), encE1, 4) // observers share one
		hub.Subscribe(e.viewer(t, "user-two"), encE1, 4)   // the same player shares one
	}

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"orc"}, At: time.Now()})

	if n := cr.n.Load(); n != 1 {
		t.Fatalf("%d definition lookups for 5 GMs, 5 observers and 5 copies of one player, want 1: the writer's request does no per-viewer reads", n)
	}
}

type noFlagChecker struct{}

func (noFlagChecker) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	return entitlements.Result{Key: key, Type: "flag", Flag: false}, nil
}

func TestLockAndInactiveFlagsAreEmittedGMOnly(t *testing.T) {
	e := newEventTableLimit(t, "50", nil)
	hub := stream.New(e.enc)
	e.sink.next = hub
	id, _, _ := privateEncounter(t, e)
	ctx := context.Background()

	before := e.sink.count()
	if err := e.enc.Items().SetInactive(ctx, "u-1", id, true); err != nil {
		t.Fatal(err)
	}
	inactive := e.sink.last()
	gated := buildEncounter("5f00000000000000000000f1", "")
	gated.SessionID, gated.GameSystemID, gated.OwnerUserID = "", "dnd5e", "u-1"
	if err := e.db.Fake(data.CollectionEncounters).InsertOne(ctx, gated); err != nil {
		t.Fatal(err)
	}
	mid := e.sink.count()
	if err := e.enc.Locks().Reconcile(ctx, "u-1", noFlagChecker{}); err != nil {
		t.Fatal(err)
	}

	if e.sink.count() <= mid || !inactive.GMOnly || inactive.Type != data.EventUpdated {
		t.Fatalf("%d changes since the inactive write (want the lock flips too); inactive change GM-only %v, type %s", e.sink.count()-before, inactive.GMOnly, inactive.Type)
	}
	// The lock reconcile locks every gated encounter the owner has, including a session's.
	for _, got := range e.sink.changes[mid:] {
		if !got.GMOnly || got.Type != data.EventUpdated {
			t.Errorf("lock change = %+v, want a GM-only encounter.updated", got)
		}
	}
}

func TestADeletedSessionsEventsReachItsSubscribersEvenThoughTheEncounterLeftTheSession(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	e.sink.next = hub
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)

	w := callAs(e.r, "user-ok", "DELETE", "/sessions/"+e.sid+"?revision=1", "")

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	for name, sub := range map[string]*stream.Subscription{"GM": gm, "player": player} {
		var released *stream.Event
		for _, ev := range drain(sub) {
			if ev.Type == data.EventReleased {
				ev := ev
				released = &ev
			}
		}
		if released == nil || len(released.Data) != 0 || released.SessionID != e.sid {
			t.Errorf("%s got %+v, want encounter.released with no data, addressed to the session it left", name, released)
		}
	}
}

func TestTheOriginOfAWriteIsTaggedFromTheClientHeaderAndTheCredential(t *testing.T) {
	e := newEventTableLimit(t, "50", nil)
	_, secret := issue(t, e.r, "user-ok", `["read","write"]`)
	body := `{"name":"N","game_system_id":"` + data.DefaultSystemID + `"}`
	post := func(token, clientID string) data.Origin {
		req := httptest.NewRequest("POST", "/encounters", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if clientID != "" {
			req.Header.Set("X-Initiative-Client-Id", clientID)
		}
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", w.Code, w.Body)
		}
		return e.sink.last().Origin
	}

	user := post("user-ok", "web-3.A_b-9")
	bad := post("user-ok", "has spaces!")
	tooLong := post("user-ok", strings.Repeat("x", 65))
	integration := post(secret, "foundry-1")

	if user.Kind != "user" || user.ClientID != "web-3.A_b-9" || user.ActorID != "u-1" {
		t.Errorf("user origin = %+v", user)
	}
	if bad.Kind != "user" || bad.ClientID != "" || tooLong.ClientID != "" {
		t.Errorf("a malformed client id = %+v / %+v, want it ignored (not rejected)", bad, tooLong)
	}
	if integration.Kind != "integration" {
		t.Errorf("a credential's write = %+v, want kind integration", integration)
	}
}

// wireHub installs a hub as the table's sink and subscribes the GM, a player and an observer.
func wireHub(t *testing.T, e eventTable) (gm, player, observer *stream.Subscription) {
	t.Helper()
	hub := stream.New(e.enc)
	e.sink.next = hub
	return hub.Subscribe(e.viewer(t, "user-ok"), encE1, 16), hub.Subscribe(e.viewer(t, "user-two"), encE1, 16), hub.Subscribe(e.viewer(t, "user-three"), encE1, 16)
}

func playerTag(t *testing.T, e eventTable) string {
	t.Helper()
	body := decodeBody(t, callAs(e.r, "user-two", "GET", encPath(e.sid, encE1), ""))
	tag, _ := body[domain.FieldViewTag].(string)
	return tag
}

func TestAHiddenOnlyEditProducesNoEventForPlayersAndObserversAndOneForTheGM(t *testing.T) {
	e := newEventTable(t, nil)
	joinAs(t, e.r, e.sid, "user-four", "co_gm")
	gm, player, observer := wireHub(t, e)

	// Controllers are GM-only data, and the hidden combatant is not in a player's view at all.
	w := callAs(e.r, "user-ok", "PUT", controllersPath(e.sid, "hid"), `{"controller_user_ids":["u-4"],"revision":3}`)

	if w.Code != 200 {
		t.Fatalf("assign = %d %s", w.Code, w.Body)
	}
	if n := len(drain(player)) + len(drain(observer)); n != 0 {
		t.Errorf("a player and an observer got %d events for an edit they cannot see; want none: no gap, no generic update", n)
	}
	got := drain(gm)
	if len(got) != 1 || got[0].Type != data.EventControllers || got[0].Revision != 4 {
		t.Fatalf("GM got %v, want one controllers.updated at revision 4", got)
	}
}

func TestAVisibleEditProducesOneEventWithAChangedViewTagAndNoRevisionForThePlayer(t *testing.T) {
	e := newEventTable(t, nil)
	gm, player, observer := wireHub(t, e)
	before := playerTag(t, e)

	w := callAs(e.r, "user-ok", "PUT", controllersPath(e.sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)

	if w.Code != 200 {
		t.Fatalf("assign = %d %s", w.Code, w.Body)
	}
	got := drain(player)
	if len(got) != 1 || got[0].Type != data.EventControllers || got[0].ViewTag == "" || got[0].ViewTag == before {
		t.Fatalf("player got %v, want one controllers.updated whose view_tag differs from %q", got, before)
	}
	if got[0].ViewTag != playerTag(t, e) {
		t.Errorf("the event's view_tag %q is not the tag of the player's view after the change %q", got[0].ViewTag, playerTag(t, e))
	}
	if strings.Contains(js(got[0]), `"revision"`) {
		t.Errorf("a player's event carries a revision: %s", js(got[0]))
	}
	if n := len(drain(observer)); n != 0 {
		t.Errorf("the observer, whose view did not change, got %d events", n)
	}
	if g := drain(gm); len(g) != 1 || g[0].Revision != 4 || g[0].ViewTag != "" {
		t.Errorf("GM got %v, want one event with revision 4 and no view_tag", g)
	}
}

func TestClosingSubscribersWhileEventsAreEmittedNeverStopsDeliveryToTheRest(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	keeper := hub.Subscribe(e.viewer(t, "user-three"), encE1, 4096)
	var closers []*stream.Subscription
	for i := 0; i < 40; i++ {
		closers = append(closers, hub.Subscribe(e.viewer(t, "user-two"), encE1, 4096))
	}
	const emits = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, s := range closers {
			s.Close()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < emits; i++ {
			hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
		}
	}()
	wg.Wait()

	if n := len(drain(keeper)); n != emits {
		t.Errorf("the open subscriber got %d of %d events: a closed subscriber must not abort delivery to the others", n, emits)
	}
}

func TestLosingAccessEndsTheMembersOpenStreamsAtOnce(t *testing.T) {
	acts := map[string]func(e eventTable) int{
		"removed": func(e eventTable) int {
			return callAs(e.r, "user-ok", "DELETE", "/sessions/"+e.sid+"/members/u-2", "").Code
		},
		"left": func(e eventTable) int { return callAs(e.r, "user-two", "POST", "/sessions/"+e.sid+"/leave", "").Code },
		"blocked": func(e eventTable) int {
			return callAs(e.r, "user-ok", "POST", "/sessions/"+e.sid+"/blocks", `{"user_id":"u-2"}`).Code
		},
		"re-roled": func(e eventTable) int {
			return callAs(e.r, "user-ok", "PATCH", "/sessions/"+e.sid+"/members/u-2", `{"role":"observer"}`).Code
		},
	}
	closed := func(s *stream.Subscription) bool {
		select {
		case _, ok := <-s.C:
			return !ok
		default:
			return false
		}
	}
	for name, act := range acts {
		t.Run(name, func(t *testing.T) {
			e := newEventTable(t, nil)
			hub := stream.New(e.enc)
			e.ses.SetEvictor(hub)
			player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
			gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)

			if code := act(e); code >= 300 {
				t.Fatalf("%s = %d", name, code)
			}

			if !closed(player) || closed(gm) {
				t.Errorf("player stream closed = %v, GM stream closed = %v; want only the player's closed", closed(player), closed(gm))
			}
		})
	}
}

func TestDeletingTheSessionEndsEveryStreamOfIt(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	e.ses.SetEvictor(hub)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)

	if w := deleteSession(e.r, "user-ok", e.sid, "1"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}

	for name, s := range map[string]*stream.Subscription{"player": player, "gm": gm} {
		for range s.C { // any release events arrive first, then the channel ends
		}
		if _, ok := <-s.C; ok {
			t.Errorf("%s stream still open after the session was deleted", name)
		}
	}
}

func TestConcurrentEmitsOnASlowSubscriberNeverSendOnAClosedChannel(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	slow := hub.Subscribe(e.viewer(t, "user-three"), encE1, 1)
	other := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 4096)
	enc := e.encounter(t)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: enc, SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
			}
		}()
	}
	wg.Wait()

	if !slow.Overflowed() {
		t.Error("the size-1 subscriber was never closed for falling behind")
	}
	if n := len(drain(other)); n != 16*50 {
		t.Errorf("the healthy subscriber got %d of %d events: a writer's panic must not abort the fan-out", n, 16*50)
	}
}

func TestSessionScopedSubscriptionsHearOfANewEncounterAndOwnerScopedOnesOfAPrivateOne(t *testing.T) {
	e := newEventTableLimit(t, "50", nil)
	hub := stream.New(e.enc)
	e.sink.next = hub
	gm := hub.Subscribe(e.viewer(t, "user-ok"), "", 16)
	player := hub.Subscribe(e.viewer(t, "user-two"), "", 16)
	owner := hub.Subscribe(ownerViewer(t, e), "", 16)

	if w := callAs(e.r, "user-ok", "POST", "/sessions/"+e.sid+"/encounters", newSessionEncounter); w.Code != http.StatusCreated {
		t.Fatalf("create in session = %d %s", w.Code, w.Body)
	}
	if w := callAs(e.r, "user-ok", "POST", "/encounters", `{"name":"P","game_system_id":"`+data.DefaultSystemID+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("create private = %d %s", w.Code, w.Body)
	}

	for name, sub := range map[string]*stream.Subscription{"GM": gm, "player": player} {
		got := drain(sub)
		if len(got) != 1 || got[0].Type != data.EventCreated || got[0].SessionID != e.sid {
			t.Errorf("%s got %v, want the one encounter.created of the session", name, got)
		}
	}
	if got := drain(owner); len(got) != 1 || got[0].Type != data.EventCreated || got[0].SessionID != "" {
		t.Errorf("the owner's private subscription got %v, want only the private encounter's creation", got)
	}
}

func ownerViewer(t *testing.T, e eventTable) data.Viewer {
	t.Helper()
	_, v, err := data.NewEncounters(e.db).LoadOwnedView(context.Background(), "u-1", privateIDFor(t, e))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// privateIDFor makes a private encounter (before any subscription exists) just to obtain an owner
// viewer for u-1.
func privateIDFor(t *testing.T, e eventTable) string {
	t.Helper()
	enc := newEncounter(t, e.r, "user-ok", `{"name":"seed","game_system_id":"`+data.DefaultSystemID+`"}`)
	return enc["id"].(string)
}

func TestARemovedPlayerAndADemotedCoGMReceiveNothingAfterwards(t *testing.T) {
	e := newEventTable(t, nil)
	joinAs(t, e.r, e.sid, "user-four", "co_gm")
	hub := stream.New(e.enc)
	e.sink.next = hub
	e.ses.SetEvictor(hub)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 16)
	coGM := hub.Subscribe(coGMViewer(t, e), encE1, 16)

	if w := callAs(e.r, "user-ok", "DELETE", "/sessions/"+e.sid+"/members/u-2", ""); w.Code >= 300 {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	drain(coGM) // the scrub's controllers.updated reached the co-GM, who was still a co-GM
	if w := callAs(e.r, "user-ok", "PATCH", "/sessions/"+e.sid+"/members/u-4", `{"role":"observer"}`); w.Code != 200 {
		t.Fatalf("demote = %d %s", w.Code, w.Body)
	}
	callAs(e.r, "user-ok", "PUT", controllersPath(e.sid, "hid"), `{"controller_user_ids":[],"revision":`+strconv.Itoa(int(gmRevision(t, e.r, e.sid)))+`}`)

	for name, sub := range map[string]*stream.Subscription{"removed player": player, "demoted co-GM": coGM} {
		if got := drain(sub); len(got) != 0 {
			t.Errorf("%s still received %v, including the scrub's own events", name, got)
		}
		if _, open := <-sub.C; open {
			t.Errorf("%s's subscription was left open", name)
		}
	}
}

// coGMViewer is the session viewer of the co-GM user-four (u-4).
func coGMViewer(t *testing.T, e eventTable) data.Viewer {
	t.Helper()
	ctx := data.WithGate(context.Background(), relations.New(e.rel.URL), "user-four")
	v, err := e.ses.Viewer(ctx, "u-4", e.sid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestASecondBusAttachReplacesTheFirstAndPrivateEncountersAreNotPublished(t *testing.T) {
	e := newEventTableLimit(t, "50", nil)
	hub := stream.New(e.enc)
	e.sink.next = hub
	first, second := &captureBus{}, &captureBus{}
	hub.AttachBus(first, 8)
	hub.AttachBus(second, 8)
	defer hub.Close()

	callAs(e.r, "user-ok", "POST", "/encounters", `{"name":"P","game_system_id":"`+data.DefaultSystemID+`"}`)
	callAs(e.r, "user-ok", "POST", "/sessions/"+e.sid+"/encounters", newSessionEncounter)

	eventually(t, func() bool { return second.count() == 1 }, "the replacement bus never got the session event")
	time.Sleep(20 * time.Millisecond)
	if first.count() != 0 || second.count() != 1 {
		t.Errorf("first bus got %d, second got %d; want 0 and exactly 1 (the private encounter is not published)", first.count(), second.count())
	}
}
