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
	"sync"
	"sync/atomic"
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
	// Close turns credentials off and persists it, so a restart stays closed until an
	// administrator's reconcile (the consumer fell behind retention).
	Close(ctx context.Context) error
	// Pause turns credentials off in this process only (an outage): a reconnect reopens it through
	// Open once the consumer has caught up, with no administrator step.
	Pause()
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

// Consumer runs the durable pull consumer on AUTH_EVENTS. A Consumer is single-use: Stop latches,
// so a failed Start is followed by a new Consumer, not a second Start.
//
// Fail closed on the credential path only (PADR-0039). A NATS outage does not stop the API: the
// client reconnects forever, and when the connection has been down longer than MaxDisconnect the
// credential gate is closed, so credentials answer 503 until the consumer is back and has caught up
// (or, if the stream's retention overtook it, until a reconcile).
type Consumer struct {
	rev  Revoker
	gate Gate
	// Stream and Filter default to AUTH_EVENTS and auth.events.>; tests point them at a prefixed stream.
	Stream, Filter string
	// MaxDisconnect is how long NATS may stay unreachable before the gate closes; DefaultMaxDisconnect
	// when zero.
	MaxDisconnect time.Duration

	conn *nats.Conn
	cc   jetstream.ConsumeContext

	stopping    atomic.Bool
	disconnects atomic.Int64

	mu           sync.Mutex
	downSince    time.Time // zero while connected
	closedByLoss bool      // the gate was closed by an outage, so a reconnect may reopen it
	now          func() time.Time
	behind       func(ctx context.Context) (bool, error) // whether retention overtook the consumer
	catchUp      func(ctx context.Context)               // reopens the gate once nothing is pending
}

// setupTimeout bounds one attempt at looking up and binding the consumer.
const setupTimeout = 30 * time.Second

// reconnectWait is the pause between reconnect tries.
const reconnectWait = 2 * time.Second

// DefaultMaxDisconnect is how long NATS may be unreachable before credentials fail closed.
const DefaultMaxDisconnect = 10 * time.Minute

// outageCheck is how often a down connection is compared with MaxDisconnect.
const outageCheck = 5 * time.Second

func (c *Consumer) onDisconnect(err error) {
	if c.stopping.Load() {
		return
	}
	n := c.disconnects.Add(1)
	c.mu.Lock()
	if c.downSince.IsZero() {
		c.downSince = c.now()
	}
	c.mu.Unlock()
	slog.Warn("lost the connection to NATS; auth events are not being applied until it returns", "disconnects", n, "error", err)
}

// DownFor is how long NATS has been unreachable (zero while connected).
func (c *Consumer) DownFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.downSince.IsZero() {
		return 0
	}
	return c.now().Sub(c.downSince)
}

func (c *Consumer) onReconnect(ctx context.Context) {
	c.mu.Lock()
	c.downSince = time.Time{}
	closed := c.closedByLoss
	c.mu.Unlock()
	slog.Info("reconnected to NATS", "disconnects", c.disconnects.Load())
	if closed {
		go c.resync(ctx)
	}
}

// onClosed runs when the client gives up for good (a rejected credential, say): the consumer is
// gone, so credentials stay closed and the cause is logged at error. The rest of the API keeps
// serving.
func (c *Consumer) onClosed(ctx context.Context) {
	if c.stopping.Load() {
		return
	}
	slog.Error("the NATS connection is closed for good; auth events can no longer be applied and integration credentials are closed", "disconnects", c.disconnects.Load())
	c.closeGate(ctx, "the NATS connection closed")
}

func (c *Consumer) closeGate(_ context.Context, _ string) {
	c.gate.Pause() // in this process only: a restart then rechecks retention itself
	c.mu.Lock()
	c.closedByLoss = true
	c.mu.Unlock()
}

// watchOutage closes the gate once the connection has been down past MaxDisconnect.
func (c *Consumer) watchOutage(ctx context.Context) {
	t := time.NewTicker(outageCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.stopping.Load() {
			return
		}
		c.checkOutage(ctx)
	}
}

func (c *Consumer) checkOutage(ctx context.Context) {
	max := c.MaxDisconnect
	if max <= 0 {
		max = DefaultMaxDisconnect
	}
	c.mu.Lock()
	already := c.closedByLoss
	c.mu.Unlock()
	if d := c.DownFor(); d > max && !already {
		slog.Error("NATS has been unreachable too long to trust the credential list; closing integration credentials", "down_for", d.String(), "limit", max.String())
		c.closeGate(ctx, "NATS unreachable")
	}
}

// resync runs after a reconnect that follows a gate closure by an outage: if the stream's retention
// overtook the consumer the gate stays closed until a reconcile; otherwise it reopens once the
// consumer has drained what was pending.
func (c *Consumer) resync(ctx context.Context) {
	behind, err := c.behind(ctx)
	if err != nil {
		slog.Error("could not check the auth-event consumer against the stream after reconnecting; credentials stay closed", "error", err)
		return
	}
	if behind {
		slog.Error("auth-event consumer is behind stream retention after the outage; integration credentials stay closed until reconcile")
		c.mu.Lock()
		c.closedByLoss = false // an administrator's reconcile reopens it, not a reconnect
		c.mu.Unlock()
		return
	}
	c.catchUp(ctx)
	c.mu.Lock()
	c.closedByLoss = false
	c.mu.Unlock()
}

// Disconnects counts the times the connection to NATS was lost since Start.
func (c *Consumer) Disconnects() int64 { return c.disconnects.Load() }

// New builds a consumer. The gate must start closed.
func New(rev Revoker, gate Gate) *Consumer {
	return &Consumer{rev: rev, gate: gate, Stream: StreamName, Filter: FilterSubject, now: time.Now}
}

// Start connects, binds the durable consumer, and begins revoking. The gate opens only after the
// consumer has drained what was pending, and stays closed when the consumer fell behind the
// stream's retention (until an administrator's reconcile reopens it). A failed Start closes its
// connection; build a new Consumer to try again.
func (c *Consumer) Start(ctx context.Context, natsURL, durable string) error {
	err := c.start(ctx, natsURL, durable)
	if err != nil {
		c.Stop()
	}
	return err
}

func (c *Consumer) start(ctx context.Context, natsURL, durable string) error {
	if durable == "" {
		durable = defaultDurable
	}
	opts := []nats.Option{
		nats.Name(durable), nats.ReconnectWait(reconnectWait),
		nats.MaxReconnects(-1), nats.RetryOnFailedConnect(true), // the outage clock, not a retry count, decides
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { c.onDisconnect(err) }),
		nats.ReconnectHandler(func(*nats.Conn) { c.onReconnect(ctx) }),
		nats.ClosedHandler(func(*nats.Conn) { c.onClosed(ctx) }),
	}
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
	// Setup calls are bounded per attempt; ctx outlives them and serves the running consumer.
	sctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	stream, err := js.Stream(sctx, c.Stream)
	if err != nil {
		return fmt.Errorf("stream %s: %w", c.Stream, err)
	}
	sinfo, err := stream.Info(sctx)
	if err != nil {
		return fmt.Errorf("stream info: %w", err)
	}
	prior, err := stream.Consumer(sctx, durable)
	exists := err == nil
	if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return fmt.Errorf("look up consumer %q: %w", durable, err)
	}
	if exists {
		pinfo, ierr := prior.Info(sctx)
		if ierr != nil {
			return fmt.Errorf("consumer info: %w", ierr)
		}
		if behindRetention(true, pinfo.AckFloor.Stream, sinfo.State.FirstSeq) {
			slog.Error("auth-event consumer is behind stream retention; integration credentials stay closed until reconcile")
			if err := c.gate.Close(sctx); err != nil {
				return fmt.Errorf("close credential gate: %w", err)
			}
		}
	}
	cons, err := stream.CreateOrUpdateConsumer(sctx, jetstream.ConsumerConfig{
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
	c.behind = func(ctx context.Context) (bool, error) {
		si, err := stream.Info(ctx)
		if err != nil {
			return false, err
		}
		ci, err := cons.Info(ctx)
		if err != nil {
			return false, err
		}
		return behindRetention(true, ci.AckFloor.Stream, si.State.FirstSeq), nil
	}
	c.catchUp = func(ctx context.Context) { c.openWhenCaughtUp(ctx, cons) }
	go c.openWhenCaughtUp(ctx, cons)
	go c.watchOutage(ctx)
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
	c.stopping.Store(true)
	if c.cc != nil {
		c.cc.Stop()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}
