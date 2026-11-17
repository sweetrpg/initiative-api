package data

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionOwnerEntitlements holds the last entitlement values read with each user's own bearer.
const CollectionOwnerEntitlements = "owner_entitlements"

// OwnerEntitlement is one recorded key: a limit or a flag, and when it was read.
type OwnerEntitlement struct {
	Key    string    `bson:"key"`
	Type   string    `bson:"type"`
	Limit  int       `bson:"limit"`
	Flag   bool      `bson:"flag"`
	ReadAt time.Time `bson:"read_at"`
}

// ownerEntitlementDoc keys entries by encoded entitlement key so each is written on its own.
type ownerEntitlementDoc struct {
	UserID  string                      `bson:"_id"`
	Entries map[string]OwnerEntitlement `bson:"entries"`
}

// entryField encodes a key for use as a document field name, which cannot contain ".".
func entryField(key string) string {
	return strings.NewReplacer("%", "%25", ".", "%2E", "$", "%24").Replace(key)
}

// OwnerEntitlements records a user's entitlement values so a request that cannot read them (an
// integration credential carries no user bearer, PADR-0034/0037) can still enforce the owner's
// limits. The record is written only by first-party requests that just read the values with the
// user's own token, and read back for that same user: one document per user, addressed by the
// caller's own ID, never by a client-supplied record ID.
type OwnerEntitlements struct {
	c      store.Collection
	maxAge time.Duration
	now    func() time.Time
}

// NewOwnerEntitlements builds the repository on db. A recorded value older than maxAge is no
// longer fresh (see Fresh).
func NewOwnerEntitlements(db store.Database, maxAge time.Duration) *OwnerEntitlements {
	return &OwnerEntitlements{c: db.Collection(CollectionOwnerEntitlements), maxAge: maxAge, now: func() time.Time { return time.Now().UTC() }}
}

// Record upserts each entry under its own field (entries.<key>) with a single atomic write, so
// concurrent recorders of different keys never revert one another.
func (r *OwnerEntitlements) Record(ctx context.Context, userID string, entries ...OwnerEntitlement) error {
	if err := checkUserID(userID); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	at := r.now()
	set := make(bson.D, 0, len(entries))
	for _, e := range entries {
		e.ReadAt = at
		set = append(set, bson.E{Key: "entries." + entryField(e.Key), Value: e})
	}
	return r.c.UpsertOne(ctx, bson.D{{Key: "_id", Value: userID}}, bson.D{{Key: "$set", Value: set}})
}

// Fresh reports whether e was read no longer than the configured max age ago.
func (r *OwnerEntitlements) Fresh(e OwnerEntitlement) bool {
	return r.now().Sub(e.ReadAt) <= r.maxAge
}

// Get returns the recorded value of key for userID, or ErrNotFound when the user has no record
// or the record lacks that key. It does not judge age; see Fresh.
func (r *OwnerEntitlements) Get(ctx context.Context, userID, key string) (OwnerEntitlement, error) {
	if err := checkUserID(userID); err != nil {
		return OwnerEntitlement{}, err
	}
	var doc ownerEntitlementDoc
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}, &doc)
	if errors.Is(err, store.ErrNotFound) {
		return OwnerEntitlement{}, ErrNotFound
	}
	if err != nil {
		return OwnerEntitlement{}, err
	}
	e, ok := doc.Entries[entryField(key)]
	if !ok {
		return OwnerEntitlement{}, ErrNotFound
	}
	return e, nil
}
