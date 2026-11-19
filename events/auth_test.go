package events

import (
	"context"
	"errors"
	"sync"
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
			if err := handle(context.Background(), r, []byte(tt.body)); err != nil {
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
	if err := handle(context.Background(), r, []byte(`{"entity_id":"auth0|a","action":"deny","data":{"service":"initiative-api"}}`)); err == nil {
		t.Fatal("failed revoke was acked; the deny would be lost")
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

func TestStartFailsWhenTheBrokerIsUnreachable(t *testing.T) {
	c := New(&rec{}, nil)
	if err := c.Start(context.Background(), "nats://127.0.0.1:1", ""); err == nil {
		t.Fatal("Start succeeded with no broker")
	}
}

type memGate struct {
	mu     sync.Mutex
	closed int
	opened int
}

func (g *memGate) Open(context.Context) (bool, error) {
	g.mu.Lock()
	g.opened++
	g.mu.Unlock()
	return true, nil
}
func (g *memGate) Pause()                      { g.mu.Lock(); g.closed++; g.mu.Unlock() }
func (g *memGate) Close(context.Context) error { g.mu.Lock(); g.closed++; g.mu.Unlock(); return nil }
func (g *memGate) counts() (int, int)          { g.mu.Lock(); defer g.mu.Unlock(); return g.closed, g.opened }

func outageConsumer(g *memGate, now *time.Time, behind bool) (*Consumer, *int) {
	c := New(nil, g)
	c.MaxDisconnect = 10 * time.Minute
	c.now = func() time.Time { return *now }
	caught := new(int)
	c.behind = func(context.Context) (bool, error) { return behind, nil }
	c.catchUp = func(context.Context) { *caught++; _, _ = g.Open(context.Background()) }
	return c, caught
}

func TestAnOutagePastTheLimitClosesTheGateButAShortOneDoesNot(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, false)
	ctx := context.Background()
	c.onDisconnect(errors.New("eof"))
	now = now.Add(9 * time.Minute)
	c.checkOutage(ctx)
	if closed, _ := g.counts(); closed != 0 {
		t.Fatalf("gate closed after 9m of a 10m limit")
	}
	now = now.Add(2 * time.Minute)
	c.checkOutage(ctx)
	c.checkOutage(ctx) // once per outage
	if closed, _ := g.counts(); closed != 1 {
		t.Errorf("gate closed %d times after 11m, want once", closed)
	}
}

func TestAReconnectInsideRetentionReopensTheGateAfterCatchUp(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, caught := outageConsumer(g, &now, false)
	c.onDisconnect(errors.New("eof"))
	now = now.Add(11 * time.Minute)
	c.checkOutage(context.Background())
	c.resync(context.Background())
	if *caught != 1 {
		t.Errorf("catch-up ran %d times, want 1", *caught)
	}
	if _, opened := g.counts(); opened != 1 {
		t.Errorf("gate opened %d times, want 1", opened)
	}
	if c.DownFor() == 0 {
		t.Log("onReconnect clears the clock") // resync alone does not
	}
}

func TestAReconnectAfterRetentionOvertookTheConsumerLeavesTheGateClosed(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, caught := outageConsumer(g, &now, true)
	c.onDisconnect(errors.New("eof"))
	now = now.Add(11 * time.Minute)
	c.checkOutage(context.Background())
	c.resync(context.Background())
	if _, opened := g.counts(); opened != 0 || *caught != 0 {
		t.Errorf("opened=%d catchups=%d, want the gate left closed until a reconcile", opened, *caught)
	}
}

func TestAPermanentCloseOfTheConnectionClosesTheGateNotTheProcess(t *testing.T) {
	g := &memGate{}
	now := time.Unix(1000, 0)
	c, _ := outageConsumer(g, &now, false)
	c.onClosed(context.Background())
	if closed, _ := g.counts(); closed != 1 {
		t.Errorf("gate closed %d times, want 1", closed)
	}
	c.Stop()
	c.onClosed(context.Background())
	if closed, _ := g.counts(); closed != 1 {
		t.Error("a deliberate Stop closed the gate")
	}
}

func TestStartAgainstNoServerFailsAndLeavesNoConnection(t *testing.T) {
	c := New(nil, &memGate{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Start(ctx, "nats://127.0.0.1:1", ""); err == nil {
		t.Fatal("Start succeeded with no server")
	}
	if c.conn != nil && !c.conn.IsClosed() {
		t.Error("a failed Start left its connection open")
	}
}
