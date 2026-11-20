package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sweetrpg/initiative-api/events/stream"
)

type fakeJS struct {
	got *nats.Msg
	err error
}

func (f *fakeJS) PublishMsg(_ context.Context, m *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.got = m
	return &jetstream.PubAck{}, f.err
}

func TestPublishSendsTheEnvelopeOnTheEntitySubjectWithTheEventIDAsDedupeKey(t *testing.T) {
	f := &fakeJS{}
	e := stream.Event{EventID: "e-1", Source: "initiative-api", SessionID: "s-1", EntityType: "encounter", EntityID: "enc-1", Action: "advanced", Type: "turn.advanced"}
	if err := (&Publisher{js: f}).Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if f.got.Subject != "initiative.events.encounter.advanced" {
		t.Errorf("subject = %q", f.got.Subject)
	}
	if f.got.Header.Get(nats.MsgIdHdr) != "e-1" {
		t.Errorf("Nats-Msg-Id = %q, want the event id", f.got.Header.Get(nats.MsgIdHdr))
	}
	var back map[string]any
	if err := json.Unmarshal(f.got.Data, &back); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"event_id": "e-1", "session_id": "s-1", "entity_id": "enc-1", "source": "initiative-api"} {
		if back[k] != want {
			t.Errorf("%s = %v, want %s", k, back[k], want)
		}
	}
}

func TestPublishReturnsTheBrokerError(t *testing.T) {
	f := &fakeJS{err: errors.New("nats: timeout")}
	if err := (&Publisher{js: f}).Publish(context.Background(), stream.Event{EntityType: "encounter", Action: "updated"}); err == nil {
		t.Fatal("a failed publication returned nil, so the hub would not count the drop")
	}
}

func TestSubjectKeepsEachValueToOneSegment(t *testing.T) {
	got := Subject(stream.Event{EntityType: "enc.>", Action: "up *"})
	if got != "initiative.events.enc__.up__" {
		t.Errorf("subject = %q", got)
	}
}
