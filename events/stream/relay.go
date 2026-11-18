package stream

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FanoutCollection is the collection pods share to reach each other's subscribers.
const FanoutCollection = "stream_fanout"

// FanoutTTL is how long a fan-out row lives. A pod that is down longer than this misses what it
// would have relayed; its clients reconnect and refetch anyway.
const FanoutTTL = 60 * time.Second

// FanoutIndexes is the TTL index of the fan-out collection.
var FanoutIndexes = []store.IndexSpec{{Name: "at_ttl", Keys: bson.D{{Key: "at", Value: 1}}, ExpireAfter: FanoutTTL}}

const (
	kindChange = "change"
	kindEvict  = "evict"
)

// fanoutDoc is one row of the fan-out collection. A change row carries the data.Change the
// encounter collection already holds the content of (the encounter document, as stored); an evict
// row carries only the session and user.
type fanoutDoc struct {
	ID      primitive.ObjectID `bson:"_id"`
	Pod     string             `bson:"pod"`
	Kind    string             `bson:"kind"`
	At      time.Time          `bson:"at"`
	Change  *data.Change       `bson:"change,omitempty"`
	Session string             `bson:"session_id,omitempty"`
	User    string             `bson:"user_id,omitempty"`
}

// Relay carries committed changes and evictions between pods through a Mongo collection: each
// commit on this pod is inserted, and every pod tails the collection with a change stream and
// delivers what other pods wrote to its own subscribers. Delivery is fail-open: when the stream
// cannot be tailed the pod keeps serving its own subscribers, logs loudly, counts the failure
// (Failures) and reports Degraded, and the tail resumes from its last token when it can.
//
// Revocation is never silent. A failed eviction insert is retried, then logged at error level and
// counted (EvictFailures); the bound then falls back to each connection's own timers (the
// heartbeat guard re-checks membership, and the maximum connection age ends every stream).
type Relay struct {
	h   *Hub
	col store.Collection
	pod string

	out  chan fanoutDoc
	done chan struct{}
	wg   sync.WaitGroup

	failures      atomic.Int64 // tail errors
	dropped       atomic.Int64 // changes not relayed (queue full or insert failed)
	evictFailures atomic.Int64 // evictions that could not be relayed
	degraded      atomic.Bool
	tailing       atomic.Bool // a change stream is open
}

// relayAttempts and relayBackoff bound the retries of one insert.
const (
	relayAttempts = 3
	relayBackoff  = 200 * time.Millisecond
	relayMaxWait  = 30 * time.Second
)

// AttachRelay makes h relay through col, with pod naming this pod (a row a pod wrote is skipped by
// that pod). queue bounds the rows waiting to be inserted. Run must be called to start tailing;
// Stop ends both.
func (h *Hub) AttachRelay(col store.Collection, pod string, queue int) *Relay {
	r := &Relay{h: h, col: col, pod: pod, out: make(chan fanoutDoc, max(queue, 1)), done: make(chan struct{})}
	h.mu.Lock()
	h.rel = r
	h.mu.Unlock()
	return r
}

func (h *Hub) relay() *Relay {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rel
}

// Failures counts the times tailing failed; Dropped the changes not relayed; EvictFailures the
// evictions not relayed to other pods. Degraded reports that the tail is down right now.
func (r *Relay) Failures() int64      { return r.failures.Load() }
func (r *Relay) Dropped() int64       { return r.dropped.Load() }
func (r *Relay) EvictFailures() int64 { return r.evictFailures.Load() }
func (r *Relay) Degraded() bool       { return r.degraded.Load() }

// Tailing reports that the change stream is open, so rows other pods write now will arrive.
func (r *Relay) Tailing() bool { return r.tailing.Load() }

// Run starts the writer and the tail. It returns at once; Stop ends them.
func (r *Relay) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.wg.Add(2)
	go func() { defer r.wg.Done(); <-r.done; cancel() }()
	go func() { defer r.wg.Done(); r.write(ctx) }()
	r.wg.Add(1)
	go func() { defer r.wg.Done(); r.tail(ctx) }()
}

// Stop ends the relay's goroutines and waits for them.
func (r *Relay) Stop() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	r.wg.Wait()
}

func (r *Relay) publishChange(c data.Change) {
	d := fanoutDoc{ID: primitive.NewObjectID(), Pod: r.pod, Kind: kindChange, At: time.Now(), Change: &c}
	select {
	case r.out <- d:
	default:
		r.dropped.Add(1)
		slog.Warn("the fan-out queue is full; other pods miss an encounter event", "encounter", c.After.ID, "type", c.Type)
	}
}

func (r *Relay) publishEvict(session, user string) {
	d := fanoutDoc{ID: primitive.NewObjectID(), Pod: r.pod, Kind: kindEvict, At: time.Now(), Session: session, User: user}
	select {
	case r.out <- d:
	default:
		r.evictFailures.Add(1)
		slog.Error("the fan-out queue is full; an eviction did not reach other pods, which end the stream on their own timers",
			"session", session, "user", user)
	}
}

func (r *Relay) write(ctx context.Context) {
	for {
		select {
		case d := <-r.out:
			r.insert(ctx, d)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Relay) insert(ctx context.Context, d fanoutDoc) {
	var err error
	for i := range relayAttempts {
		if err = r.col.InsertOne(ctx, d); err == nil {
			return
		}
		select {
		case <-time.After(relayBackoff << i):
		case <-ctx.Done():
			return
		}
	}
	if d.Kind == kindEvict {
		r.evictFailures.Add(1)
		slog.Error("an eviction did not reach other pods, which end the stream on their own timers",
			"session", d.Session, "user", d.User, "error", err)
		return
	}
	r.dropped.Add(1)
	slog.Warn("relaying an encounter event to other pods failed", "type", d.Change.Type, "error", err)
}

// tail follows the collection until ctx ends, resuming from the last token after an error and
// from "now" when the token is refused (it aged out), so a failure costs missed events (clients
// refetch) and never the pod.
func (r *Relay) tail(ctx context.Context) {
	var token bson.Raw
	wait := time.Second
	for ctx.Err() == nil {
		cur, err := store.Watch(ctx, r.col, token)
		if err != nil {
			r.fail(ctx, "cannot watch the fan-out collection; this pod delivers local events only", err)
			if token != nil {
				token = nil // possibly aged out of the oplog; start from now
			}
			wait = r.sleep(ctx, wait)
			continue
		}
		// The position at open, so a failure before the first row still resumes from here.
		if t := cur.Token(); t != nil {
			token = t
		}
		r.degraded.Store(false)
		r.tailing.Store(true)
		for cur.Next(ctx) {
			wait = time.Second
			token = cur.Token()
			r.handle(ctx, cur.Doc())
		}
		r.tailing.Store(false)
		err = cur.Err()
		_ = cur.Close(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return
		}
		r.fail(ctx, "the fan-out change stream ended; resuming", err)
		wait = r.sleep(ctx, wait)
	}
}

func (r *Relay) fail(ctx context.Context, msg string, err error) {
	r.failures.Add(1)
	r.degraded.Store(true)
	slog.ErrorContext(ctx, msg, "error", err, "failures", r.failures.Load())
}

// sleep waits out the backoff and returns the next one.
func (r *Relay) sleep(ctx context.Context, d time.Duration) time.Duration {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	return min(d*2, relayMaxWait)
}

func (r *Relay) handle(ctx context.Context, raw bson.Raw) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "handling a fan-out row panicked", "panic", p)
		}
	}()
	var d fanoutDoc
	if err := bson.Unmarshal(raw, &d); err != nil {
		slog.WarnContext(ctx, "a fan-out row could not be read", "error", err)
		return
	}
	if d.Pod == r.pod {
		return
	}
	switch {
	case d.Kind == kindEvict:
		r.h.evictLocal(d.Session, d.User)
	case d.Kind == kindChange && d.Change != nil:
		r.h.emitRemote(ctx, *d.Change)
	}
}
