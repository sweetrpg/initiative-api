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

func TestAnObserverEventCarriesViewTagAndNoRevision(t *testing.T) {
	f := &fakeJS{}
	e := stream.Event{EventID: "e-2", EntityType: "encounter", Action: "updated", ViewTag: "tag-9", Revision: 0}
	if err := (&Publisher{js: f}).Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(f.got.Data, &back); err != nil {
		t.Fatal(err)
	}
	if back["view_tag"] != "tag-9" {
		t.Errorf("view_tag = %v, want tag-9", back["view_tag"])
	}
	if _, has := back["revision"]; has {
		t.Errorf("revision present in %s; observer events must not carry it", f.got.Data)
	}
}

func TestPublishReturnsTheBrokerError(t *testing.T) {
	f := &fakeJS{err: errors.New("nats: timeout")}
	if err := (&Publisher{js: f}).Publish(context.Background(), stream.Event{EntityType: "encounter", Action: "updated"}); err == nil {
		t.Fatal("a failed publication returned nil, so the hub would not count the drop")
	}
}

func TestAnAddressedEventGoesOnItsUsersSubjectKeepingTheDottedType(t *testing.T) {
	got := Subject(stream.Event{UserID: "auth0|u1", Type: "turn.yours", EntityType: "encounter", Action: "yours"})
	if got != "initiative.user.auth0|u1.turn.yours" {
		t.Errorf("subject = %q", got)
	}
	if got := Subject(stream.Event{UserID: "u.1 *", Type: "change_request.submitted"}); got != "initiative.user.u_1__.change_request.submitted" {
		t.Errorf("an unsafe user id changed the subject shape: %q", got)
	}
}

func TestSubjectKeepsEachValueToOneSegment(t *testing.T) {
	got := Subject(stream.Event{EntityType: "enc.>", Action: "up *"})
	if got != "initiative.events.enc__.up__" {
		t.Errorf("subject = %q", got)
	}
}
