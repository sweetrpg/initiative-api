package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sweetrpg/initiative-api/events/stream"
)

// SubjectPrefix is the root of initiative's entity-change subjects:
// initiative.events.<entity>.<action> (docs/messaging-nats.md, PADR-0026). The INITIATIVE_EVENTS
// stream binds initiative.events.>.
const SubjectPrefix = "initiative.events."

// jsPublisher is the part of jetstream.JetStream the publisher uses.
type jsPublisher interface {
	PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// Publisher puts encounter events on the platform bus. It implements stream.Bus; the hub calls it
// from its own queue, so a slow or absent broker never reaches a write (fail-open, PADR-0024).
// There is no retry queue: an event that cannot be published is dropped and counted by the hub.
type Publisher struct {
	js   jsPublisher
	conn *nats.Conn
}

// NewPublisher connects to natsURL with the login from the environment (NATS_CREDS, or NATS_USER
// and NATS_PASSWORD). The client keeps retrying a broker that is down at start and after a loss, so
// the service starts either way; a publication made meanwhile fails and is dropped.
func NewPublisher(natsURL string) (*Publisher, error) {
	opts := append([]nats.Option{
		nats.Name("initiative-api-publisher"), nats.ReconnectWait(reconnectWait),
		nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true),
	}, authOptions()...)
	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create JetStream context: %w", err)
	}
	return &Publisher{js: js, conn: conn}, nil
}

// UserSubjectPrefix is the root of the subjects of events addressed to one user:
// initiative.user.<user_id>.<type>, where the dotted type (turn.yours) stays two tokens, so a
// consumer filters with initiative.user.*.turn.>.
const UserSubjectPrefix = "initiative.user."

// Subject is the subject an event is published on.
func Subject(e stream.Event) string {
	if e.UserID != "" {
		parts := strings.Split(e.Type, ".")
		for i := range parts {
			parts[i] = token(parts[i])
		}
		return UserSubjectPrefix + token(e.UserID) + "." + strings.Join(parts, ".")
	}
	return SubjectPrefix + token(e.EntityType) + "." + token(e.Action)
}

// token keeps a value to one subject segment; a dot, wildcard or space would change the subject.
func token(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '*', '>', ' ':
			return '_'
		}
		return r
	}, s)
}

// Publish sends e and waits for the stream's acknowledgement, bounded by ctx. event_id doubles as
// the Nats-Msg-Id, so a republished event is stored once inside the stream's duplicate window.
func (p *Publisher) Publish(ctx context.Context, e stream.Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event %s: %w", e.EventID, err)
	}
	msg := &nats.Msg{Subject: Subject(e), Data: body, Header: nats.Header{}}
	msg.Header.Set(nats.MsgIdHdr, e.EventID)
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", msg.Subject, err)
	}
	return nil
}

// Close flushes what the client still has buffered and closes the connection. Call it after the
// hub's queue has stopped (Hub.Close), so events the queue already handed over are not lost on a
// deploy. A broker that is down makes Drain give up; the events are then dropped, as in any outage.
func (p *Publisher) Close() {
	if err := p.conn.Drain(); err != nil {
		p.conn.Close()
	}
}
