package data

import (
	"context"
	"errors"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionOwnerEntitlements holds the last entitlement values read with each user's own bearer.
const CollectionOwnerEntitlements = "owner_entitlements"

// OwnerEntitlement is one recorded key: a limit or a flag.
type OwnerEntitlement struct {
	Key   string `bson:"key"`
	Type  string `bson:"type"`
	Limit int    `bson:"limit"`
	Flag  bool   `bson:"flag"`
}

type ownerEntitlementDoc struct {
	UserID  string             `bson:"_id"`
	Entries []OwnerEntitlement `bson:"entries"`
	ReadAt  time.Time          `bson:"read_at"`
}

// OwnerEntitlements records a user's entitlement values so a request that cannot read them (an
// integration credential carries no user bearer, PADR-0034/0037) can still enforce the owner's
// limits. The record is written only by first-party requests that just read the values with the
// user's own token, and read back for that same user: one document per user, addressed by the
// caller's own ID, never by a client-supplied record ID.
type OwnerEntitlements struct {
	c   store.Collection
	now func() time.Time
}

// NewOwnerEntitlements builds the repository on db.
func NewOwnerEntitlements(db store.Database) *OwnerEntitlements {
	return &OwnerEntitlements{c: db.Collection(CollectionOwnerEntitlements), now: func() time.Time { return time.Now().UTC() }}
}

// Record merges entries into userID's record and stamps the read time. Concurrent records can
// drop one another's keys; the next first-party read restores them, so this is best effort.
func (r *OwnerEntitlements) Record(ctx context.Context, userID string, entries ...OwnerEntitlement) error {
	if err := checkUserID(userID); err != nil {
		return err
	}
	var cur ownerEntitlementDoc
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}, &cur)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	merged := map[string]OwnerEntitlement{}
	for _, e := range cur.Entries {
		merged[e.Key] = e
	}
	for _, e := range entries {
		merged[e.Key] = e
	}
	list := make([]OwnerEntitlement, 0, len(merged))
	for _, e := range merged {
		list = append(list, e)
	}
	doc := ownerEntitlementDoc{UserID: userID, Entries: list, ReadAt: r.now()}
	if errors.Is(err, store.ErrNotFound) {
		if ierr := r.c.InsertOne(ctx, doc); !errors.Is(ierr, store.ErrDuplicate) {
			return ierr
		}
	}
	var discard ownerEntitlementDoc
	return r.c.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: userID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "entries", Value: list}, {Key: "read_at", Value: doc.ReadAt}}}}, &discard)
}

// Get returns the recorded value of key for userID and when it was read, or ErrNotFound when the
// user has no record or the record lacks that key.
func (r *OwnerEntitlements) Get(ctx context.Context, userID, key string) (OwnerEntitlement, time.Time, error) {
	if err := checkUserID(userID); err != nil {
		return OwnerEntitlement{}, time.Time{}, err
	}
	var doc ownerEntitlementDoc
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}, &doc)
	if errors.Is(err, store.ErrNotFound) {
		return OwnerEntitlement{}, time.Time{}, ErrNotFound
	}
	if err != nil {
		return OwnerEntitlement{}, time.Time{}, err
	}
	for _, e := range doc.Entries {
		if e.Key == key {
			return e, doc.ReadAt, nil
		}
	}
	return OwnerEntitlement{}, time.Time{}, ErrNotFound
}
