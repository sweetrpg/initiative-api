package data

import (
	"context"
	"errors"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionStreamLeases holds one row per open (or just dropped) event stream, so a user's
// concurrent streams can be counted across pods.
const CollectionStreamLeases = "stream_leases"

// ErrStreamLimit means the user already holds the most concurrent streams allowed.
var ErrStreamLimit = errors.New("data: too many concurrent streams")

// leaseRowTTL keeps rows past their lease for a while before the TTL index removes them; the
// counts filter on Until, never on the index.
const leaseRowTTL = time.Minute

var leaseIndexes = []store.IndexSpec{
	{Name: "until_ttl", Keys: bson.D{{Key: "until", Value: 1}}, ExpireAfter: leaseRowTTL},
	{Name: "user_until", Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "until", Value: 1}}},
}

// StreamLease is one stream's seat. Until is when the seat lapses unless renewed: a pod that
// crashes leaves seats that expire on their own, and a dropped client keeps its seat for a grace
// period so a reconnect does not need a free one.
type StreamLease struct {
	ID        string    `bson:"_id"`
	UserID    string    `bson:"user_id"`
	SessionID string    `bson:"session_id,omitempty"`
	ClientID  string    `bson:"client_id,omitempty"`
	Pod       string    `bson:"pod"`
	Until     time.Time `bson:"until"`
	Dropped   bool      `bson:"dropped,omitempty"`
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

// Acquire gives l's user a seat valid for ttl, or ErrStreamLimit when they already hold max. A
// dropped seat of the same client id is taken over first (a reconnect keeps its seat). Two
// acquisitions can both pass the count, so the seat is recounted after the insert and given back
// if the user is over: both may then be refused, but neither is kept over the limit.
// ponytail: concurrent connects at the limit can all be refused; a retry succeeds.
func (r *StreamLeases) Acquire(ctx context.Context, l StreamLease, ttl time.Duration, max int) error {
	if err := checkUserID(l.UserID); err != nil {
		return err
	}
	if l.ClientID != "" {
		_ = r.c.DeleteOne(ctx, bson.D{{Key: "user_id", Value: l.UserID}, {Key: "client_id", Value: l.ClientID}, {Key: "dropped", Value: true}})
	}
	l.Until, l.Dropped = r.now().Add(ttl), false
	if err := r.c.InsertOne(ctx, l); err != nil {
		return err
	}
	n, err := r.c.CountDocuments(ctx, bson.D{{Key: "user_id", Value: l.UserID}, {Key: "until", Value: bson.D{{Key: "$gt", Value: r.now()}}}})
	if err == nil && n > int64(max) {
		err = ErrStreamLimit
	}
	if err != nil {
		_ = r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: l.ID}})
		return err
	}
	return nil
}

// Renew pushes the seat's end to ttl from now.
func (r *StreamLeases) Renew(ctx context.Context, id string, ttl time.Duration) error {
	var out StreamLease
	return r.c.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "until", Value: r.now().Add(ttl)}}}}, &out)
}

// Release marks the seat dropped, held for grace so a reconnect of the same client keeps it. With
// no grace, or no client id to reconnect by, it is removed.
func (r *StreamLeases) Release(ctx context.Context, id, clientID string, grace time.Duration) error {
	if grace <= 0 || clientID == "" {
		return ignoreMissing(r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}))
	}
	var out StreamLease
	return ignoreMissing(r.c.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "dropped", Value: true}, {Key: "until", Value: r.now().Add(grace)}}}}, &out))
}

func ignoreMissing(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// Count is how many seats the user holds now.
func (r *StreamLeases) Count(ctx context.Context, userID string) (int64, error) {
	return r.c.CountDocuments(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "until", Value: bson.D{{Key: "$gt", Value: r.now()}}}})
}
