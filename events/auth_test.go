package events

import (
	"context"
	"errors"
	"testing"

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
