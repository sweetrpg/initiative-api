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

// Evictor closes every open stream of a user, on this pod and the others. *stream.Hub implements
// it.
type Evictor interface {
	EvictUser(userID string)
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
//
// A deny for this service or a removed role ends the subject's integration credentials and then
// every open stream of that user (PADR-0039: a denied owner's open event streams close when the
// revocation lands); ev may be nil. Credentials are revoked first and a failure there is retried
// before anything is evicted; the eviction is idempotent, so a redelivery repeats it harmlessly.
func handle(ctx context.Context, rev Revoker, evict Evictor, body []byte) error {
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
	if evict != nil {
		evict.EvictUser(ev.EntityID)
	}
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
	// Evictor, when set, closes a denied user's open streams (see handle).
	Evictor Evictor
	// MaxDisconnect is how long NATS may stay unreachable before the gate closes; DefaultMaxDisconnect
	// when zero.
	MaxDisconnect time.Duration

	conn *nats.Conn
	cc   jetstream.ConsumeContext

	stopping    atomic.Bool
	disconnects atomic.Int64
	resyncing   atomic.Bool
	dead        chan struct{}
	deadOnce    sync.Once
	stopOnce    sync.Once
	sleep       func(time.Duration)

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
	c.mu.Unlock()
	slog.Info("reconnected to NATS", "disconnects", c.disconnects.Load())
	go c.resync(ctx) // every reconnect: a gap shorter than MaxDisconnect can still outlast the stream's retention
}

// onClosed runs when the client gives up for good (a rejected credential, say): this consumer is
// gone, so credentials are paused and Dead is signalled; the owner builds a new Consumer. The rest of
// the API keeps serving.
func (c *Consumer) onClosed() {
	if c.stopping.Load() {
		return
	}
	slog.Error("the NATS connection is closed for good; auth events can no longer be applied and integration credentials are closed", "disconnects", c.disconnects.Load())
	c.closeGate()
	c.deadOnce.Do(func() { close(c.dead) })
}

// Dead is closed when the connection to NATS is lost for good. The consumer cannot recover; build
// another with New and Start it.
func (c *Consumer) Dead() <-chan struct{} { return c.dead }

func (c *Consumer) closeGate() {
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
	limit := c.MaxDisconnect
	if limit <= 0 {
		limit = DefaultMaxDisconnect
	}
	c.mu.Lock()
	already := c.closedByLoss
	c.mu.Unlock()
	if d := c.DownFor(); d > limit && !already {
		slog.Error("NATS has been unreachable too long to trust the credential list; closing integration credentials", "down_for", d.String(), "limit", limit.String())
		c.closeGate()
	}
}

// resync runs after every reconnect, one at a time. It asks whether the stream's retention overtook
// this consumer while it was away: if so a deny may be lost for good, so the gate is closed for
// everyone (persisted, the way startup does it) until a reconcile. Otherwise, if an outage had paused
// credentials, they reopen once the consumer has drained what was pending. A failed check or close is
// retried with a growing wait until it succeeds or the consumer stops.
func (c *Consumer) resync(ctx context.Context) {
	if !c.resyncing.CompareAndSwap(false, true) {
		return // one already running; it will see the current state
	}
	defer c.resyncing.Store(false)
	wait := time.Second
	for !c.stopping.Load() && ctx.Err() == nil {
		if c.resyncOnce(ctx) {
			return
		}
		c.sleep(wait)
		wait = min(wait*2, 30*time.Second)
	}
}

// ready returns the stream-bound checks, nil until start has bound the consumer. A reconnect can
// fire while start is still waiting on JetStream (RetryOnFailedConnect); start's own checks cover
// that window.
func (c *Consumer) ready() (behind func(context.Context) (bool, error), catchUp func(context.Context)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.behind, c.catchUp
}

// closeIfBehind persists the gate closure when retention overtook the consumer. done is true once
// the question is settled (the gate was closed, or the check does not apply yet); a false done with
// behind false means the consumer is within retention.
func (c *Consumer) closeIfBehind(ctx context.Context) (behind, done bool) {
	check, _ := c.ready()
	if check == nil {
		return false, true
	}
	behind, err := check(ctx)
	if err != nil {
		slog.Error("could not check the auth-event consumer against the stream; will retry", "error", err)
		return false, false
	}
	if !behind {
		return false, true
	}
	slog.Error("auth-event consumer is behind stream retention; closing integration credentials until reconcile")
	if err := c.gate.Close(ctx); err != nil {
		slog.Error("could not persist the credential gate closure; will retry", "error", err)
		return true, false
	}
	c.mu.Lock()
	c.closedByLoss = false // a reconcile reopens it, not a reconnect
	c.mu.Unlock()
	return true, true
}

// resyncOnce reports whether the check is finished.
func (c *Consumer) resyncOnce(ctx context.Context) bool {
	behind, done := c.closeIfBehind(ctx)
	if !done || behind {
		return done
	}
	_, catchUp := c.ready()
	if catchUp == nil {
		return true
	}
	c.mu.Lock()
	reopen := c.closedByLoss
	c.mu.Unlock()
	if reopen {
		catchUp(ctx)
		c.mu.Lock()
		c.closedByLoss = false
		c.mu.Unlock()
	}
	return true
}

// New builds a consumer. The gate must start closed.
func New(rev Revoker, gate Gate) *Consumer {
	return &Consumer{rev: rev, gate: gate, Stream: StreamName, Filter: FilterSubject, now: time.Now, sleep: time.Sleep, dead: make(chan struct{})}
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
		nats.ClosedHandler(func(*nats.Conn) { c.onClosed() }),
	}
	opts = append(opts, authOptions()...)
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
	behind := func(ctx context.Context) (bool, error) {
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
	catchUp := func(ctx context.Context) { c.openWhenCaughtUp(ctx, cons) }
	c.mu.Lock()
	c.behind, c.catchUp = behind, catchUp
	c.mu.Unlock()
	go catchUp(ctx)
	go c.watchOutage(ctx)
	return nil
}

func (c *Consumer) process(ctx context.Context, msg jetstream.Msg) {
	if err := handle(ctx, c.rev, c.Evictor, msg.Data()); err != nil {
		slog.Error("auth event handling failed, will retry", "error", err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

// openWhenCaughtUp polls until nothing is pending or awaiting ack, then opens the gate. Each round
// rechecks retention first, so a poll that outlives an outage cannot open a gate the stream's
// retention has since closed; it ends when the consumer stops.
func (c *Consumer) openWhenCaughtUp(ctx context.Context, cons jetstream.Consumer) {
	t := time.NewTicker(time.Second)
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
		if behind, done := c.closeIfBehind(ctx); behind && done {
			return
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

// Stop halts consumption and closes the connection. It is safe to call more than once and from
// several goroutines (the shutdown path and the Dead path can both reach it).
func (c *Consumer) Stop() {
	c.stopping.Store(true)
	c.stopOnce.Do(func() {
		if c.cc != nil {
			c.cc.Stop()
		}
		if c.conn != nil {
			c.conn.Close()
		}
	})
}

// authOptions is the broker login from the environment: a NATS_CREDS file when set, else
// NATS_USER and NATS_PASSWORD.
func authOptions() []nats.Option {
	if creds := os.Getenv("NATS_CREDS"); creds != "" {
		return []nats.Option{nats.UserCredentials(creds)}
	}
	if user := os.Getenv("NATS_USER"); user != "" {
		return []nats.Option{nats.UserInfo(user, os.Getenv("NATS_PASSWORD"))}
	}
	return nil
}
