//go:build integration

package events

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type fakeGate struct{ opened chan struct{} }

func (g fakeGate) Open(context.Context) (bool, error) { close(g.opened); return true, nil }
func (g fakeGate) Close(context.Context) error        { return nil }

type chanRevoker struct{ got chan [2]string }

func (r chanRevoker) RevokeOwner(_ context.Context, o, reason string) (int, error) {
	r.got <- [2]string{o, reason}
	return 1, nil
}

// TestNATSConsumerRevokesAndOpens runs against NATS_TEST_URL with NATS_USER/NATS_PASSWORD. It
// creates one stream named TEST_<nanotime>_AUTH_EVENTS on its own subject prefix and deletes only that.
func TestNATSConsumerRevokesAndOpens(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL not set")
	}
	id := time.Now().UnixNano()
	stream, prefix := fmt.Sprintf("TEST_%d_AUTH_EVENTS", id), fmt.Sprintf("test%d.auth.events", id)
	opts := []nats.Option{nats.UserInfo(os.Getenv("NATS_USER"), os.Getenv("NATS_PASSWORD"))}
	conn, err := nats.Connect(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	js, _ := jetstream.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: jetstream.MemoryStorage}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()
	// Published before the consumer exists: replay must still revoke.
	if _, err := js.Publish(ctx, prefix+".deny", []byte(`{"event_id":"e1","entity_id":"auth0|a","action":"deny"}`)); err != nil {
		t.Fatal(err)
	}
	rev, gate := chanRevoker{make(chan [2]string, 4)}, fakeGate{make(chan struct{})}
	c := New(rev, gate)
	c.Stream, c.Filter = stream, prefix+".>"
	if err := c.Start(ctx, url, fmt.Sprintf("test-%d", id)); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	select {
	case got := <-rev.got:
		if got != [2]string{"auth0|a", "owner_denied"} {
			t.Fatalf("revoked %v", got)
		}
	case <-ctx.Done():
		t.Fatal("no revoke from replayed event")
	}
	select {
	case <-gate.opened:
	case <-ctx.Done():
		t.Fatal("gate never opened after catch-up")
	}
	if _, err := js.Publish(ctx, prefix+".role_removed", []byte(`{"event_id":"e2","entity_id":"auth0|b","action":"role_removed"}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-rev.got; got != [2]string{"auth0|b", "owner_role_removed"} {
		t.Fatalf("live event revoked %v", got)
	}
}
