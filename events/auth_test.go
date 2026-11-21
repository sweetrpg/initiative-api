package events

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type rec struct {
	owner, reason string
	err           error
}

func (r *rec) RevokeOwner(_ context.Context, owner, reason string) (int, error) {
	r.owner, r.reason = owner, reason
	return 1, r.err
}

func TestHandleMapsKindToReason(t *testing.T) {
	tests := []struct{ name, body, owner, reason string }{
		{"deny", `{"event_id":"e1","entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`, "auth0|a", "owner_denied"},
		{"role removed", `{"event_id":"e2","entity_id":"auth0|b","action":"role_removed"}`, "auth0|b", "owner_role_removed"},
		{"deny of another service", `{"entity_id":"auth0|a","action":"deny","data":{"service":"catalog-api"}}`, "", ""},
		{"deny without a service", `{"entity_id":"auth0|a","action":"deny"}`, "", ""},
		{"unrelated action", `{"entity_id":"auth0|c","action":"role_added"}`, "", ""},
		{"no subject", `{"action":"deny"}`, "", ""},
		{"not json", `nope`, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &rec{}
			if err := handle(context.Background(), r, nil, []byte(tt.body)); err != nil {
				t.Fatalf("err = %v, want ack", err)
			}
			if r.owner != tt.owner || r.reason != tt.reason {
				t.Fatalf("revoked %q/%q, want %q/%q", r.owner, r.reason, tt.owner, tt.reason)
			}
		})
	}
}

func TestHandleFailedRevokeIsRetried(t *testing.T) {
	r := &rec{err: errors.New("db down")}
	if err := handle(context.Background(), r, nil, []byte(`{"entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`)); err == nil {
		t.Fatal("failed revoke was acked; the deny would be lost")
	}
}

type evicted struct{ users []string }

func (e *evicted) EvictUser(u string) { e.users = append(e.users, u) }

func TestADenyOrRemovedRoleEvictsTheUsersStreamsAfterTheCredentialsAreRevoked(t *testing.T) {
	tests := []struct {
		name, body string
		want       []string
	}{
		{"deny for this service", `{"entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`, []string{"auth0|a"}},
		{"role removed", `{"entity_id":"auth0|b","action":"role_removed"}`, []string{"auth0|b"}},
		{"deny of another service", `{"entity_id":"auth0|a","action":"deny","data":{"service":"catalog-api"}}`, nil},
		{"role added", `{"entity_id":"auth0|c","action":"role_added"}`, nil},
		{"no subject", `{"action":"deny","data":{"service":"initiative-api"}}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &evicted{}
			if err := handle(context.Background(), &rec{}, e, []byte(tt.body)); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(e.users, tt.want) {
				t.Errorf("evicted %v, want %v", e.users, tt.want)
			}
		})
	}
}

func TestAFailedRevokeEvictsNothingSoTheRedeliveryDoesBoth(t *testing.T) {
	e := &evicted{}
	err := handle(context.Background(), &rec{err: errors.New("db down")}, e, []byte(`{"entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`))
	if err == nil || len(e.users) != 0 {
		t.Errorf("err = %v, evicted %v; want the error returned and no eviction before the credentials are revoked", err, e.users)
	}
}

func TestBehindRetention(t *testing.T) {
	tests := []struct {
		name            string
		exists          bool
		ackFloor, first uint64
		want            bool
	}{
		{"new consumer", false, 0, 500, false},
		{"caught up", true, 99, 100, false},
		{"stream never trimmed", true, 0, 1, false},
		{"events trimmed unseen", true, 40, 100, true},
		{"never acked, stream trimmed", true, 0, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := behindRetention(tt.exists, tt.ackFloor, tt.first); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeMsg records whether the consumer acknowledged or refused a message.
type fakeMsg struct {
	jetstream.Msg
	body       string
	acked, nak bool
}

func (m *fakeMsg) Data() []byte { return []byte(m.body) }
func (m *fakeMsg) Ack() error   { m.acked = true; return nil }
func (m *fakeMsg) Nak() error   { m.nak = true; return nil }

func TestProcessAcksHandledEventsAndNaksFailures(t *testing.T) {
	deny := `{"entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`
	tests := []struct {
		name      string
		revokeErr error
		wantAck   bool
	}{
		{"revoked", nil, true},
		{"revoke fails, redelivered later", errors.New("db down"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(&rec{err: tt.revokeErr}, nil)
			m := &fakeMsg{body: deny}

			c.process(context.Background(), m)

			if m.acked != tt.wantAck || m.nak == tt.wantAck {
				t.Fatalf("acked = %v, nak = %v, want acked = %v", m.acked, m.nak, tt.wantAck)
			}
		})
	}
}

func TestNewDefaultsToTheAuthStreamAndStopIsSafeBeforeStart(t *testing.T) {
	c := New(&rec{}, nil)

	c.Stop()

	if c.Stream != StreamName || c.Filter != FilterSubject {
		t.Fatalf("stream %q filter %q, want %q %q", c.Stream, c.Filter, StreamName, FilterSubject)
	}
}

type memGate struct {
	mu        sync.Mutex
	paused    int
	persisted int
	opened    int
	closeErr  error
}

func (g *memGate) Open(context.Context) (bool, error) {
	g.mu.Lock()
	g.opened++
	g.mu.Unlock()
	return true, nil
}
func (g *memGate) Pause() { g.mu.Lock(); g.paused++; g.mu.Unlock() }
func (g *memGate) Close(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closeErr != nil {
		return g.closeErr
	}
	g.persisted++
	return nil
}
func (g *memGate) counts() (paused, persisted, opened int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused, g.persisted, g.opened
}

func outageConsumer(g *memGate, now *time.Time, behind bool) (*Consumer, *int) {
	c := New(nil, g)
	c.MaxDisconnect = 10 * time.Minute
	c.now = func() time.Time { return *now }
	c.sleep = func(time.Duration) {}
	caught := new(int)
	c.behind = func(context.Context) (bool, error) { return behind, nil }
	c.catchUp = func(context.Context) { *caught++; _, _ = g.Open(context.Background()) }
	return c, caught
}

func TestAnOutagePastTheLimitPausesCredentialsButAShortOneDoesNot(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, false)
	ctx := context.Background()
	c.onDisconnect(errors.New("eof"))
	now = now.Add(9 * time.Minute)
	c.checkOutage(ctx)
	if paused, _, _ := g.counts(); paused != 0 {
		t.Fatalf("paused after 9m of a 10m limit")
	}
	now = now.Add(2 * time.Minute)
	c.checkOutage(ctx)
	c.checkOutage(ctx) // once per outage
	if paused, persisted, _ := g.counts(); paused != 1 || persisted != 0 {
		t.Errorf("paused=%d persisted=%d after 11m, want one pause and nothing persisted", paused, persisted)
	}
}

func TestAReconnectInsideRetentionReopensAPausedGateAfterCatchUp(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, caught := outageConsumer(g, &now, false)
	c.onDisconnect(errors.New("eof"))
	now = now.Add(11 * time.Minute)
	c.checkOutage(context.Background())
	c.onReconnect(context.Background())
	waitUntil(t, func() bool { _, _, o := g.counts(); return o == 1 })
	if *caught != 1 || c.DownFor() != 0 {
		t.Errorf("catchups=%d downFor=%s, want 1 and 0", *caught, c.DownFor())
	}
}

func TestEveryReconnectChecksRetentionEvenWhenTheGateWasNeverPaused(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, true) // retention overtook the consumer during a short gap
	c.onDisconnect(errors.New("eof"))
	now = now.Add(time.Minute)
	c.onReconnect(context.Background())
	waitUntil(t, func() bool { _, p, _ := g.counts(); return p == 1 })
}

func TestAReconnectAfterRetentionOvertookTheConsumerPersistsTheClosure(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, caught := outageConsumer(g, &now, true)
	c.onDisconnect(errors.New("eof"))
	now = now.Add(11 * time.Minute)
	c.checkOutage(context.Background())
	c.resync(context.Background())
	if _, persisted, opened := g.counts(); persisted != 1 || opened != 0 || *caught != 0 {
		t.Errorf("persisted=%d opened=%d catchups=%d, want a persisted close and nothing reopened", persisted, opened, *caught)
	}
}

func TestAFailedRetentionCheckOrCloseIsRetriedUntilItSucceeds(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, true)
	calls := 0
	c.behind = func(context.Context) (bool, error) {
		if calls++; calls < 3 {
			return false, errors.New("nats: timeout")
		}
		return true, nil
	}
	g.closeErr = errors.New("mongo down")
	c.sleep = func(time.Duration) { g.mu.Lock(); g.closeErr = nil; g.mu.Unlock() } // the store recovers during the wait
	c.resync(context.Background())
	if _, persisted, _ := g.counts(); calls < 3 || persisted != 1 {
		t.Errorf("checks=%d persisted=%d, want the check retried and the close persisted", calls, persisted)
	}
}

func TestOnlyOneResyncRunsAtATime(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, false)
	release := make(chan struct{})
	var runs atomic.Int32
	c.behind = func(context.Context) (bool, error) { runs.Add(1); <-release; return false, nil }
	first := make(chan struct{})
	go func() { defer close(first); c.resync(context.Background()) }()
	waitUntil(t, func() bool { return runs.Load() == 1 }) // the first one now holds the slot
	var others sync.WaitGroup
	for range 4 {
		others.Add(1)
		go func() { defer others.Done(); c.resync(context.Background()) }()
	}
	others.Wait() // each returned at the slot check while the first was still inside behind
	close(release)
	<-first
	if runs.Load() != 1 {
		t.Errorf("%d resyncs checked retention, want 1", runs.Load())
	}
}

func TestAReconnectBeforeStartHasBoundTheConsumerIsHarmless(t *testing.T) {
	c := New(nil, &memGate{}) // behind and catchUp are unset, as while start waits on JetStream
	c.onReconnect(context.Background())
	c.resync(context.Background()) // would panic on a nil func
}

func TestStopIsIdempotentAndSafeConcurrently(t *testing.T) {
	c := New(nil, &memGate{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); c.Stop() }()
	}
	wg.Wait()
	c.Stop()
	if !c.stopping.Load() {
		t.Error("Stop did not latch")
	}
}

func TestAPermanentCloseOfTheConnectionPausesCredentialsAndSignalsDeadButNotAfterStop(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, false)
	c.onClosed()
	select {
	case <-c.Dead():
	default:
		t.Error("Dead was not signalled")
	}
	if paused, _, _ := g.counts(); paused != 1 {
		t.Errorf("paused %d times, want 1", paused)
	}
	d := New(nil, g)
	d.Stop()
	d.onClosed()
	select {
	case <-d.Dead():
		t.Error("a deliberate Stop signalled Dead")
	default:
	}
}

func TestStartAgainstNoServerFailsAndLeavesNoConnection(t *testing.T) {
	c := New(nil, &memGate{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := c.Start(ctx, "nats://127.0.0.1:1", ""); err == nil {
		t.Fatal("Start succeeded with no server")
	}
	if c.conn != nil && !c.conn.IsClosed() {
		t.Error("a failed Start left its connection open")
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
