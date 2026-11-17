package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	data.EventTurn, data.EventCombatantAd, data.EventCombatantUp, data.EventCombatantRm, data.EventCombatantRs, data.EventControllers,
}

func TestNoEventAPlayerOrObserverCanReceiveCarriesAHiddenCombatant(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	enc := e.encounter(t) // the hidden adversary "hid" is the current turn
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 64)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 64)
	observer := hub.Subscribe(e.viewer(t, "user-three"), encE1, 64)
	secrets := []string{`"hid"`, secretName, secretNote, `"initiative":`, `"health"`, `:31`}

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
	// The GM is told everything, hidden ids included.
	if body := js(drain(gm)); !strings.Contains(body, `"hid"`) {
		t.Errorf("the GM never saw the hidden combatant's id: %s", body)
	}
}

func TestAPlayerSeesTheVisibleIdsOfAChangeThatAlsoTouchedAHiddenOne(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(e.enc)
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUp, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid", "orc"}, At: time.Now()})
	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUp, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid"}, At: time.Now()})
	hub.Emit(context.Background(), data.Change{Type: data.EventTurn, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", At: time.Now()})
	got := drain(player)

	if len(got) != 3 {
		t.Fatalf("%d events", len(got))
	}
	if ids, _ := got[0].Data["combatant_ids"].([]string); got[0].Type != data.EventCombatantUp || len(ids) != 1 || ids[0] != "orc" {
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
	time.Sleep(50 * time.Millisecond)
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

type failingRenderer struct{}

func (failingRenderer) Render(context.Context, data.Encounter, data.Viewer) (domain.EncounterView, error) {
	return nil, errors.New("no system definition")
}

func TestAnEventFailsClosedWhenAViewerCannotBeRendered(t *testing.T) {
	e := newEventTable(t, nil)
	hub := stream.New(failingRenderer{})
	player := hub.Subscribe(e.viewer(t, "user-two"), encE1, 8)
	gm := hub.Subscribe(e.viewer(t, "user-ok"), encE1, 8)

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUp, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"hid", "orc"}, At: time.Now()})

	p := drain(player)
	if len(p) != 1 || p[0].Type != data.EventUpdated || len(p[0].Data) != 0 || strings.Contains(js(p), "orc") {
		t.Fatalf("player got %v, want the generic encounter.updated with no ids, never the unfiltered event", p)
	}
	if g := drain(gm); len(g) != 1 || g[0].Type != data.EventCombatantUp {
		t.Errorf("the GM, who needs no rendering, got %v", g)
	}
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
	time.Sleep(50 * time.Millisecond)

	g, p := drain(gm), drain(player)
	if g[0].Origin.Kind != "user" || g[0].Origin.ClientID != "ios-7.f" {
		t.Errorf("the originating user's stream = %+v, want their own client id", g[0].Origin)
	}
	if p[0].Origin.Kind != "user" || p[0].Origin.ClientID != "" {
		t.Errorf("another user's stream = %+v, want the kind and no client id", p[0].Origin)
	}
	if bus.count() != 1 || bus.events[0].Origin.ClientID != "" {
		t.Errorf("the bus form carries a client id: %v", bus.events)
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

func (c *countingRenderer) Render(ctx context.Context, enc data.Encounter, v data.Viewer) (domain.EncounterView, error) {
	c.n.Add(1)
	return c.inner.Render(ctx, enc, v)
}

func TestAnEventIsRenderedOncePerDistinctViewNotOncePerSubscriber(t *testing.T) {
	e := newEventTable(t, nil)
	cr := &countingRenderer{inner: e.enc}
	hub := stream.New(cr)
	for i := 0; i < 5; i++ {
		hub.Subscribe(e.viewer(t, "user-ok"), encE1, 4)    // GM: needs no rendering
		hub.Subscribe(e.viewer(t, "user-three"), encE1, 4) // observers share one
		hub.Subscribe(e.viewer(t, "user-two"), encE1, 4)   // the same player shares one
	}

	hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUp, After: e.encounter(t), SessionID: e.sid, OwnerID: "u-1", CombatantID: []string{"orc"}, At: time.Now()})

	if n := cr.n.Load(); n != 2 {
		t.Fatalf("%d renderings for 5 GMs, 5 observers and 5 copies of one player, want 2 (observer, player)", n)
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
