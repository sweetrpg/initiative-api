package stream

import (
	"context"
	"errors"
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

// FanoutTTL is how long a fan-out row is meant to live. The TTL monitor removes expired rows about
// once a minute, so a row can outlive it by up to two minutes in all. A pod that is down longer than
// that misses what it would have relayed; its tail then restarts without a position and ends every
// local stream (see Relay). Changing it makes the index creation conflict with the existing one, so
// startup fails until the index is dropped.
const FanoutTTL = 60 * time.Second

// MaxStreamAge and GuardInterval are the values the stream transport (5.2 part 2) enforces; they
// live here so the relay's documentation can state the bounds. They are the bounds on revocation when a relayed eviction is lost: a
// connection ends at MaxStreamAge whatever happens, and its guard re-checks membership every
// GuardInterval. The transport that enforces both is 5.2 part 2 (the stream endpoint, branch
// feature/stream-tickets); until it merges, only the relay's own retries and tail-gap eviction apply.
const (
	MaxStreamAge  = time.Hour
	GuardInterval = 30 * time.Second
)

// FanoutIndexes is the TTL index of the fan-out collection.
var FanoutIndexes = []store.IndexSpec{{Name: "at_ttl", Keys: bson.D{{Key: "at", Value: 1}}, ExpireAfter: FanoutTTL}}

const (
	kindChange = "change"
	kindEvict  = "evict"
	kindNotice = "notice"
)

// fanoutDoc is one row of the fan-out collection. A change row carries the data.Change the
// encounter collection already holds the content of (the encounter document, as stored); an evict
// row carries only the session and user.
type fanoutDoc struct {
	ID      primitive.ObjectID `bson:"_id"`
	Pod     string             `bson:"pod"`
	Kind    string             `bson:"kind"`
	At      time.Time          `bson:"at"`
	EventID string             `bson:"event_id,omitempty"`
	Change  *data.Change       `bson:"change,omitempty"`
	Notice  *data.Notice       `bson:"notice,omitempty"`
	Session string             `bson:"session_id,omitempty"`
	User    string             `bson:"user_id,omitempty"`
}

// Relay carries committed changes and evictions between pods through a Mongo collection: each
// commit on this pod is inserted, and every pod tails the collection with a change stream and
// delivers what other pods wrote to its own subscribers. Delivery is fail-open: when the stream
// cannot be tailed the pod keeps serving its own subscribers, logs loudly, counts the failure
// (Failures) and reports Degraded.
//
// Revocation is never silent. Evictions have their own queue and writer, so a slow insert of
// changes never delays one; each insert has a timeout and a failed one is retried for up to
// evictWindow, then logged at error level and counted (EvictFailures). The tail resumes from its
// last token after a transient error; only when the server says the history is gone (or the tail
// never had a position) does it restart from now, and then it ends every local subscription
// (GapEvictions), because evictions written in the gap are unknown and each client must reconnect
// and be authorized again. A change row dropped because this pod's remote queue is full ends that
// change's audience here for the same reason. Rows lost on the writing side (a full change queue,
// or an insert that failed through its window) cannot be recovered by the other pods, which learn
// of the gap only from clients refetching. The final bound for anything lost beyond that is MaxStreamAge, with
// membership re-checked every GuardInterval.
type Relay struct {
	h   *Hub
	col store.Collection
	pod string

	out    chan fanoutDoc // changes to insert
	evicts chan fanoutDoc // evictions to insert, on their own writer
	in     chan fanoutDoc // change rows from other pods, rendered off the tail goroutine
	done   chan struct{}
	once   sync.Once
	ran    atomic.Bool
	flight sync.WaitGroup // evictions queued or being inserted
	wg     sync.WaitGroup

	failures      atomic.Int64 // tail errors, ever
	streak        atomic.Int64 // tail errors since a row last arrived; only the first logs at error level
	dropped       atomic.Int64 // changes not relayed or not delivered (queue full or insert failed)
	evictFailures atomic.Int64 // evictions that could not be relayed
	gapEvictions  atomic.Int64 // times the tail restarted without a position and ended every stream
	degraded      atomic.Bool
	tailing       atomic.Bool // a change stream is open
}

const (
	insertTimeout = 5 * time.Second
	changeWindow  = 2 * time.Second  // retry window for a change row
	evictWindow   = 30 * time.Second // retry window for an eviction row
	relayBackoff  = 200 * time.Millisecond
	relayMaxWait  = 30 * time.Second
	drainWait     = 2 * time.Second // Stop's wait for queued evictions
)

// AttachRelay makes h relay through col, with pod naming this pod (a row a pod wrote is skipped by
// that pod, so the name must be unique per process). queue bounds the rows waiting to be inserted.
// Run must be called to start tailing; Stop ends both.
func (h *Hub) AttachRelay(col store.Collection, pod string, queue int) *Relay {
	q := max(queue, 1)
	r := &Relay{h: h, col: col, pod: pod, done: make(chan struct{}),
		out: make(chan fanoutDoc, q), evicts: make(chan fanoutDoc, q), in: make(chan fanoutDoc, q)}
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
// evictions not relayed to other pods; GapEvictions the times every local stream was ended after a
// gap in the tail. Degraded reports that the tail is down right now.
func (r *Relay) Failures() int64      { return r.failures.Load() }
func (r *Relay) Dropped() int64       { return r.dropped.Load() }
func (r *Relay) EvictFailures() int64 { return r.evictFailures.Load() }
func (r *Relay) GapEvictions() int64  { return r.gapEvictions.Load() }
func (r *Relay) Degraded() bool       { return r.degraded.Load() }

// Tailing reports that the change stream is open, so rows other pods write now will arrive.
func (r *Relay) Tailing() bool { return r.tailing.Load() }

// Run starts the writers, the delivery loop and the tail. It returns at once; Stop ends them.
func (r *Relay) Run(ctx context.Context) {
	r.ran.Store(true)
	ctx, cancel := context.WithCancel(ctx)
	r.wg.Add(1)
	go func() { defer r.wg.Done(); <-r.done; cancel() }()
	for _, f := range []func(context.Context){r.writeChanges, r.writeEvicts, r.deliverRemote, r.tail} {
		r.wg.Add(1)
		go func() { defer r.wg.Done(); f(ctx) }()
	}
}

// Stop ends the relay's goroutines and waits for them. Evictions still queued get drainWait to be
// inserted first, so a shutdown does not lose a revocation.
func (r *Relay) Stop() {
	r.once.Do(func() {
		if r.ran.Load() {
			// Wait for every eviction queued or mid-insert, bounded by drainWait.
			drained := make(chan struct{})
			go func() { r.flight.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(drainWait):
			}
		}
		close(r.done)
	})
	r.wg.Wait()
}

func (r *Relay) publishChange(c data.Change, eventID string) {
	d := fanoutDoc{ID: primitive.NewObjectID(), Pod: r.pod, Kind: kindChange, At: time.Now(), EventID: eventID, Change: &c}
	select {
	case r.out <- d:
	default:
		r.dropped.Add(1)
		slog.Warn("the fan-out queue is full; other pods miss an encounter event", "encounter", c.After.ID, "type", c.Type)
	}
}

// publishNotice carries an addressed event to the other pods, whose streams may hold its
// addressees. Like a change it is dropped, with a log line, when the queue is full.
func (r *Relay) publishNotice(n data.Notice) {
	d := fanoutDoc{ID: primitive.NewObjectID(), Pod: r.pod, Kind: kindNotice, At: time.Now(), Notice: &n}
	select {
	case r.out <- d:
	default:
		r.dropped.Add(1)
		slog.Warn("the fan-out queue is full; other pods miss a notification", "type", n.Type, "session", n.SessionID)
	}
}

func (r *Relay) publishEvict(session, user string) {
	d := fanoutDoc{ID: primitive.NewObjectID(), Pod: r.pod, Kind: kindEvict, At: time.Now(), Session: session, User: user}
	r.flight.Add(1)
	select {
	case r.evicts <- d:
	default:
		r.flight.Done()
		r.evictFailures.Add(1)
		slog.Error("the eviction queue is full; an eviction did not reach other pods, which end the stream on their own timers",
			"session", session, "user", user)
	}
}

func (r *Relay) writeChanges(ctx context.Context) { r.writeFrom(ctx, r.out, changeWindow, nil) }
func (r *Relay) writeEvicts(ctx context.Context) {
	r.writeFrom(ctx, r.evicts, evictWindow, r.flight.Done)
}

func (r *Relay) writeFrom(ctx context.Context, q <-chan fanoutDoc, window time.Duration, done func()) {
	for {
		select {
		case d := <-q:
			r.insert(ctx, d, window)
			if done != nil {
				done()
			}
		case <-ctx.Done():
			return
		}
	}
}

// insert writes d, each attempt under insertTimeout, retrying with backoff while window lasts. It
// does not sleep after the last attempt.
func (r *Relay) insert(ctx context.Context, d fanoutDoc, window time.Duration) {
	start, wait := time.Now(), relayBackoff
	var err error
	for {
		ictx, cancel := context.WithTimeout(ctx, insertTimeout)
		err = r.col.InsertOne(ictx, d)
		cancel()
		if err == nil {
			return
		}
		if ctx.Err() != nil || time.Since(start)+wait > window {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		wait = min(wait*2, 2*time.Second)
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

// tail follows the collection until ctx ends. After a transient error it resumes from its last
// token. A token the server cannot resume (store.ErrHistoryLost) is dropped, and whenever the tail
// opens without a position after a failure, every local subscription ends (see Relay).
func (r *Relay) tail(ctx context.Context) {
	var token bson.Raw
	failed := false
	wait := time.Second
	for ctx.Err() == nil {
		cur, err := store.Watch(ctx, r.col, token)
		if err != nil {
			failed = true
			r.fail(ctx, "cannot watch the fan-out collection; this pod delivers local events only", err)
			if errors.Is(err, store.ErrHistoryLost) {
				token = nil
			}
			wait = r.sleep(ctx, wait)
			continue
		}
		if token == nil && failed {
			n := r.h.evictAll()
			r.gapEvictions.Add(1)
			slog.ErrorContext(ctx, "the fan-out tail restarted without a position; every stream on this pod was ended", "streams", n)
		}
		// The position at open, so a failure before the first row still resumes from here.
		if t := cur.Token(); t != nil && token == nil {
			token = t
		}
		r.degraded.Store(false)
		r.tailing.Store(true)
		for cur.Next(ctx) {
			wait = time.Second
			r.streak.Store(0)
			token = cur.Token()
			r.handle(ctx, cur.Doc())
		}
		r.tailing.Store(false)
		err = cur.Err()
		_ = cur.Close(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return
		}
		failed = true
		if errors.Is(err, store.ErrHistoryLost) {
			token = nil
		}
		r.fail(ctx, "the fan-out change stream ended; resuming", err)
		wait = r.sleep(ctx, wait)
	}
}

func (r *Relay) fail(ctx context.Context, msg string, err error) {
	r.failures.Add(1)
	n := r.streak.Add(1)
	r.degraded.Store(true)
	if n > 1 && ctx.Err() == nil {
		slog.WarnContext(ctx, msg, "error", err, "failures", n) // repeated: a standalone server logs once at error level
		return
	}
	slog.ErrorContext(ctx, msg, "error", err, "failures", n)
}

// sleep waits out the backoff and returns the next one.
func (r *Relay) sleep(ctx context.Context, d time.Duration) time.Duration {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	return min(d*2, relayMaxWait)
}

// handle routes a row from the tail. An eviction is applied here at once; a change row is queued for
// deliverRemote, so rendering never delays the next eviction row.
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
	case d.Kind == kindEvict && d.Session == "" && d.User != "":
		r.h.evictUserLocal(d.User) // a platform-wide eviction: every stream of that user
	case d.Kind == kindEvict && d.Session == "":
		slog.WarnContext(ctx, "dropped a fan-out eviction with no session and no user")
	case d.Kind == kindEvict:
		r.h.evictLocal(d.Session, d.User)
	case d.Kind == kindNotice && d.Notice != nil:
		r.h.enqueueNotice(noticeJob{n: *d.Notice, local: true}) // preferences are read on the worker, never on the tail
	case d.Kind == kindChange && d.Change != nil:
		select {
		case r.in <- d:
		default:
			// A view_tag-suppressed stream hides a gap, so the audience must reconnect and refetch.
			r.dropped.Add(1)
			r.h.evictAudience(*d.Change)
			slog.ErrorContext(ctx, "the remote delivery queue is full; the change's audience on this pod was disconnected to refetch")
		}
	}
}

func (r *Relay) deliverRemote(ctx context.Context) {
	for {
		select {
		case d := <-r.in:
			r.deliverOne(ctx, d)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Relay) deliverOne(ctx context.Context, d fanoutDoc) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "delivering a fan-out row panicked", "panic", p)
		}
	}()
	r.h.emitRemote(ctx, *d.Change, d.EventID)
}
