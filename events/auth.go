// Package events consumes auth-api's change events from the platform bus (PADR-0039): a deny or
// a role removal ends the subject's integration credentials. The event carries no token and the
// consumer holds no service credential (PADR-0037).
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sweetrpg/initiative-api/data"
)

const (
	// StreamName is auth-api's JetStream stream, subject auth.events.>.
	StreamName     = "AUTH_EVENTS"
	FilterSubject  = "auth.events.>"
	defaultDurable = "initiative-api-credential-revocation"
	// Change kinds the event's action carries.
	kindDeny        = "deny"
	kindRoleRemoved = "role_removed"
)

// event is the platform envelope; entity_id is the Auth0 subject and action the change kind.
// Unknown fields are ignored so the envelope can grow.
type event struct {
	EventID  string `json:"event_id"`
	EntityID string `json:"entity_id"`
	Action   string `json:"action"`
	Data     struct {
		Service string `json:"service"`
	} `json:"data"`
}

// ServiceID is the name auth-api's deny entries use for this service. A deny of any other service
// says nothing about initiative credentials.
const ServiceID = "initiative-api"

// Revoker ends a subject's credentials. domain.Credentials implements it.
type Revoker interface {
	RevokeOwner(ctx context.Context, owner, reason string) (int, error)
}

// Gate is the on/off switch for integration credentials. domain.Credentials implements it.
type Gate interface {
	Open(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
}

// handle applies one message. A nil error means ack it; an unparseable or irrelevant message is
// acked (redelivery cannot fix it), a failed revoke is returned so the bus redelivers.
func handle(ctx context.Context, rev Revoker, body []byte) error {
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		slog.Error("dropping unparseable auth event", "error", err)
		return nil
	}
	reason := map[string]string{kindDeny: data.ReasonOwnerDenied, kindRoleRemoved: data.ReasonOwnerRoleRemoved}[ev.Action]
	if reason == "" || ev.EntityID == "" || (ev.Action == kindDeny && ev.Data.Service != ServiceID) {
		return nil
	}
	n, err := rev.RevokeOwner(ctx, ev.EntityID, reason)
	if err != nil {
		return fmt.Errorf("revoke credentials for event %s: %w", ev.EventID, err)
	}
	slog.Info("revoked integration credentials", "event_id", ev.EventID, "reason", reason, "count", n)
	return nil
}

// behindRetention reports whether a consumer that last acknowledged ackFloor can no longer replay
// the gap because the stream's oldest retained message is firstSeq. A consumer that never existed
// has nothing to miss: credentials are not issued before it is bound.
func behindRetention(exists bool, ackFloor, firstSeq uint64) bool {
	return exists && firstSeq > 1 && ackFloor+1 < firstSeq
}

// Consumer runs the durable pull consumer on AUTH_EVENTS.
type Consumer struct {
	rev  Revoker
	gate Gate
	// Stream and Filter default to AUTH_EVENTS and auth.events.>; tests point them at a prefixed stream.
	Stream, Filter string
	conn           *nats.Conn
	cc             jetstream.ConsumeContext
}

// New builds a consumer. The gate must start closed.
func New(rev Revoker, gate Gate) *Consumer {
	return &Consumer{rev: rev, gate: gate, Stream: StreamName, Filter: FilterSubject}
}

// Start connects, binds the durable consumer, and begins revoking. The gate opens only after the
// consumer has drained what was pending, and stays closed when the consumer fell behind the
// stream's retention (until an administrator's reconcile reopens it).
func (c *Consumer) Start(ctx context.Context, natsURL, durable string) error {
	if durable == "" {
		durable = defaultDurable
	}
	opts := []nats.Option{nats.Name(durable)}
	if creds := os.Getenv("NATS_CREDS"); creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
	} else if user := os.Getenv("NATS_USER"); user != "" {
		opts = append(opts, nats.UserInfo(user, os.Getenv("NATS_PASSWORD")))
	}
	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	c.conn = conn
	js, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("create JetStream context: %w", err)
	}
	stream, err := js.Stream(ctx, c.Stream)
	if err != nil {
		return fmt.Errorf("stream %s: %w", c.Stream, err)
	}
	sinfo, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("stream info: %w", err)
	}
	prior, err := stream.Consumer(ctx, durable)
	exists := err == nil
	if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return fmt.Errorf("look up consumer %q: %w", durable, err)
	}
	if exists {
		pinfo, ierr := prior.Info(ctx)
		if ierr != nil {
			return fmt.Errorf("consumer info: %w", ierr)
		}
		if behindRetention(true, pinfo.AckFloor.Stream, sinfo.State.FirstSeq) {
			slog.Error("auth-event consumer is behind stream retention; integration credentials stay closed until reconcile")
			if err := c.gate.Close(ctx); err != nil {
				return fmt.Errorf("close credential gate: %w", err)
			}
		}
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: durable, FilterSubject: c.Filter, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return fmt.Errorf("bind durable consumer %q: %w", durable, err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) { c.process(ctx, msg) })
	if err != nil {
		return fmt.Errorf("start consuming: %w", err)
	}
	c.cc = cc
	go c.openWhenCaughtUp(ctx, cons)
	return nil
}

func (c *Consumer) process(ctx context.Context, msg jetstream.Msg) {
	if err := handle(ctx, c.rev, msg.Data()); err != nil {
		slog.Error("auth event handling failed, will retry", "error", err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

// openWhenCaughtUp polls until nothing is pending or awaiting ack, then opens the gate.
func (c *Consumer) openWhenCaughtUp(ctx context.Context, cons jetstream.Consumer) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		info, err := cons.Info(ctx)
		if err != nil || info.NumPending > 0 || info.NumAckPending > 0 {
			continue
		}
		open, err := c.gate.Open(ctx)
		if err != nil {
			slog.Error("open credential gate", "error", err)
			continue
		}
		slog.Info("integration credentials caught up", "open", open)
		return
	}
}

// Stop halts consumption and closes the connection.
func (c *Consumer) Stop() {
	if c.cc != nil {
		c.cc.Stop()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}
