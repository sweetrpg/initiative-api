package data

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionStreamLeases holds one row per open (or just dropped) event stream, so a user's
// concurrent streams can be counted across pods.
const CollectionStreamLeases = "stream_leases"

// ErrStreamLimit means the user already holds the most concurrent streams allowed.
var ErrStreamLimit = errors.New("data: too many concurrent streams")

// DeviceLimitError means one member already holds the most concurrent connections to a session.
type DeviceLimitError struct{ Limit int }

func (e *DeviceLimitError) Error() string {
	return fmt.Sprintf("data: a member may hold at most %d connections to a session", e.Limit)
}

// ErrStreamUnavailable means the seat store could not be reached or written, so no seat could be
// granted. A new connect fails closed on it; streams already open stay up (see Renew).
var ErrStreamUnavailable = errors.New("data: stream seats are unavailable")

// leaseRowTTL keeps rows past their lease for a while before the TTL index removes them; the
// counts filter on Until, never on the index.
const leaseRowTTL = time.Minute

// cleanupTimeout bounds the writes that undo or release a seat, which must finish even when the
// request that took the seat is gone.
const cleanupTimeout = 5 * time.Second

var leaseIndexes = []store.IndexSpec{
	{Name: "until_ttl", Keys: bson.D{{Key: "until", Value: 1}}, ExpireAfter: leaseRowTTL},
	{Name: "bucket_until", Keys: bson.D{{Key: "bucket", Value: 1}, {Key: "until", Value: 1}}},
}

// StreamLease is one stream's seat. Until is when the seat lapses unless renewed: a pod that
// crashes leaves seats that expire on their own, and a dropped client keeps its seat for a grace
// period so a reconnect does not need a free one. Bucket is what the cap counts: "user:<id>" for a
// user's stream, "cred:<id>" for an integration credential's, so a runaway credential cannot lock
// its owner out of their own stream. Every pod stamps Until from its own clock and the pods compare
// them, so the cluster's clocks must agree to within a few seconds (a skew moves a seat's lapse by
// that much). Pod and SessionID are not read yet; the per-session member count (7.12b) will.
type StreamLease struct {
	ID        string    `bson:"_id"`
	Bucket    string    `bson:"bucket"`
	UserID    string    `bson:"user_id"`
	SessionID string    `bson:"session_id,omitempty"`
	ClientID  string    `bson:"client_id,omitempty"`
	Pod       string    `bson:"pod"`
	Until     time.Time `bson:"until"`
	Dropped   bool      `bson:"dropped,omitempty"`
	// Counted marks a session seat that counts toward the session's member limit: every member
	// but the GM. Every session seat, the GM's too, counts toward the member's device limit.
	Counted bool `bson:"counted,omitempty"`
}

// StreamLeases is the lease store.
type StreamLeases struct {
	c   store.Collection
	now func() time.Time
}

// NewStreamLeases returns the lease store on db.
func NewStreamLeases(db store.Database) *StreamLeases {
	return &StreamLeases{c: db.Collection(CollectionStreamLeases), now: time.Now}
}

// SetClock replaces the clock seats are aged by (tests).
func (r *StreamLeases) SetClock(now func() time.Time) { r.now = now }

func (r *StreamLeases) live(l StreamLease) bson.D {
	f := bson.D{{Key: "bucket", Value: l.Bucket}, {Key: "until", Value: bson.D{{Key: "$gt", Value: r.now()}}}}
	if l.ClientID != "" {
		// A dropped seat of the same user and client is the one this stream takes over, not a second
		// seat. The user matters: a session bucket is shared, so another member's client id must not
		// hide this user's grace seat.
		f = append(f, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "dropped", Value: bson.D{{Key: "$ne", Value: true}}}},
			bson.D{{Key: "client_id", Value: bson.D{{Key: "$ne", Value: l.ClientID}}}},
			bson.D{{Key: "user_id", Value: bson.D{{Key: "$ne", Value: l.UserID}}}},
		}})
	}
	return f
}

// retract removes seat id even when the request that took it has ended.
func (r *StreamLeases) retract(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("could not give back a stream seat; it lapses on its own", "seat", id, "error", err)
	}
}

// Acquire gives l a seat valid for ttl, or ErrStreamLimit when its bucket already holds max, or
// ErrStreamUnavailable when the store fails. Two acquisitions can both pass the count, so the seat
// is recounted after the insert and given back if the bucket is over: both may then be refused, but
// neither is kept over the limit. A dropped seat of the same client is taken over once the new one
// is in, not before, so a refused reconnect keeps its old seat.
// ponytail: concurrent connects at the limit can all be refused; a retry succeeds.
func (r *StreamLeases) Acquire(ctx context.Context, l StreamLease, ttl time.Duration, max int) error {
	if err := checkUserID(l.UserID); err != nil {
		return err
	}
	if l.Bucket == "" {
		l.Bucket = "user:" + l.UserID
	}
	l.Until, l.Dropped = r.now().Add(ttl), false
	if err := r.c.InsertOne(ctx, l); err != nil {
		r.retract(ctx, l.ID) // the write may have landed before the error
		return fmt.Errorf("%w: %v", ErrStreamUnavailable, err)
	}
	n, err := r.c.CountDocuments(ctx, r.live(l))
	if err != nil {
		r.retract(ctx, l.ID)
		return fmt.Errorf("%w: %v", ErrStreamUnavailable, err)
	}
	if n > int64(max) {
		r.retract(ctx, l.ID)
		return ErrStreamLimit
	}
	if l.ClientID != "" {
		r.takeOver(ctx, l)
	}
	return nil
}

// AcquireSession gives l a seat in a session's bucket. A member's connections share one member
// seat, so the session holds at most members distinct counted members (a dropped client's seat
// counts until its grace ends), and one member holds at most devices live connections, the GM
// included. A new connection of a member who already holds a seat adds no member, so it is refused
// only by the device limit, even when the member limit has fallen below the members connected.
// Over the device limit is *DeviceLimitError, over the member limit ErrStreamLimit, a store failure
// ErrStreamUnavailable. Like Acquire, the seat is inserted, recounted and given back when over, so
// two racing first connects of different members can both be refused but neither is kept over.
func (r *StreamLeases) AcquireSession(ctx context.Context, l StreamLease, ttl time.Duration, members, devices int) error {
	if err := checkUserID(l.UserID); err != nil {
		return err
	}
	l.Until, l.Dropped = r.now().Add(ttl), false
	if err := r.c.InsertOne(ctx, l); err != nil {
		r.retract(ctx, l.ID)
		return fmt.Errorf("%w: %v", ErrStreamUnavailable, err)
	}
	var rows []StreamLease
	err := r.c.Find(ctx, bson.D{{Key: "bucket", Value: l.Bucket}, {Key: "until", Value: bson.D{{Key: "$gt", Value: r.now()}}}}, store.FindOptions{}, &rows)
	if err != nil {
		r.retract(ctx, l.ID)
		return fmt.Errorf("%w: %v", ErrStreamUnavailable, err)
	}
	if err := checkSessionSeat(l, rows, members, devices); err != nil {
		r.retract(ctx, l.ID)
		return err
	}
	if l.ClientID != "" {
		r.takeOver(ctx, l)
	}
	return nil
}

// checkSessionSeat applies the device and member limits to rows, the live seats of l's bucket
// including l.
func checkSessionSeat(l StreamLease, rows []StreamLease, members, devices int) error {
	mine, distinct := 0, map[string]bool{}
	for _, row := range rows {
		if row.UserID == l.UserID && !row.Dropped {
			mine++
		}
		if row.Counted && (row.UserID != l.UserID || row.ID == l.ID) {
			distinct[row.UserID] = true
		}
	}
	if mine > devices {
		return &DeviceLimitError{Limit: devices}
	}
	if !l.Counted || len(distinct) <= members {
		return nil
	}
	for _, row := range rows {
		if row.UserID == l.UserID && row.ID != l.ID && row.Counted {
			return nil // the member already holds a seat; another connection adds no member
		}
	}
	return ErrStreamLimit
}

// takeOver removes every dropped seat of l's user and client other than l, however many two streams that
// shared a client id left behind.
func (r *StreamLeases) takeOver(ctx context.Context, l StreamLease) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for range 8 {
		err := r.c.DeleteOne(ctx, bson.D{{Key: "bucket", Value: l.Bucket}, {Key: "client_id", Value: l.ClientID}, {Key: "user_id", Value: l.UserID}, {Key: "dropped", Value: true}, {Key: "_id", Value: bson.D{{Key: "$ne", Value: l.ID}}}})
		if err != nil {
			return // none left, or the store failed: the stale seat lapses on its own
		}
	}
}

// ponytail: Renew can bring back a seat that lapsed (after more than the lease TTL of failed
// renewals) without recounting, so a bucket may sit one over its cap until that stream ends; a
// recount here would close a healthy stream on a store blip.
// Renew pushes the seat's end to ttl from now. If the row is gone (its TTL ran out while renewals
// were failing) it is put back, so a healthy stream does not lose its seat. A renewal that fails
// leaves the stream up; its seat lapses after the lease TTL if renewals keep failing.
func (r *StreamLeases) Renew(ctx context.Context, l StreamLease, ttl time.Duration) error {
	var out StreamLease
	err := r.c.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: l.ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "until", Value: r.now().Add(ttl)}}}}, &out)
	if errors.Is(err, store.ErrNotFound) {
		l.Until, l.Dropped = r.now().Add(ttl), false
		return r.c.InsertOne(ctx, l)
	}
	return err
}

// Release ends the seat. With keep, a client that has an id keeps it for grace so its reconnect
// does not need a free seat; every other end (evicted, over its age, revoked, never opened) frees
// it at once.
func (r *StreamLeases) Release(ctx context.Context, l StreamLease, grace time.Duration, keep bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if !keep || grace <= 0 || l.ClientID == "" {
		return ignoreMissing(r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: l.ID}}))
	}
	var out StreamLease
	return ignoreMissing(r.c.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: l.ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "dropped", Value: true}, {Key: "until", Value: r.now().Add(grace)}}}}, &out))
}

func ignoreMissing(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
