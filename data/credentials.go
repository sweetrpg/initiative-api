package data

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// Credentials is the repository for integration credentials: the owner-scoped operations come
// from Owned, plus the state changes (revoke, use) and the one lookup that is not owner-scoped.
type Credentials struct {
	*Owned[IntegrationCredential, *IntegrationCredential]
	gate, deletions store.Collection
}

// NewCredentials builds the credential repository for db.
func NewCredentials(db store.Database) *Credentials {
	return &Credentials{NewOwned[IntegrationCredential](db, CredentialSpec), db.Collection(CollectionCredentialGate), db.Collection(CollectionCredentialDeletions)}
}

// activeScope is owner's credentials that are neither revoked nor deleted.
func activeScope(owner string) bson.D {
	return append(scope(owner), bson.E{Key: "revoked_at", Value: nil})
}

// ItemStates lists owner's unrevoked credentials; a revoked one no longer counts toward the limit.
func (r *Credentials) ItemStates(ctx context.Context, owner string) ([]ItemState, error) {
	if err := checkUserID(owner); err != nil {
		return nil, err
	}
	return r.itemStates(ctx, activeScope(owner))
}

// CountActive counts owner's credentials that can still authenticate.
func (r *Credentials) CountActive(ctx context.Context, owner string) (int64, error) {
	if err := checkUserID(owner); err != nil {
		return 0, err
	}
	return r.c.CountDocuments(ctx, activeScope(owner))
}

// Revoke ends owner's credential at once. Revoking an already revoked credential succeeds and
// returns it unchanged. Another owner's credential is ErrForbidden, an unknown one ErrNotFound.
func (r *Credentials) Revoke(ctx context.Context, owner, actor, id string) (IntegrationCredential, error) {
	var out IntegrationCredential
	if err := requireOwnerActor(owner, actor); err != nil {
		return out, err
	}
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	now := r.now()
	update := bson.D{
		{Key: "$set", Value: bson.D{{Key: "revoked_at", Value: now}, {Key: "updated_at", Value: now}, {Key: "updated_by", Value: actor}}},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	filter := append(scopeID(owner, id), bson.E{Key: "revoked_at", Value: nil})
	err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
	if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	// Nothing matched: already revoked (fine), someone else's, or missing.
	cur, gerr := r.Get(ctx, owner, id)
	if gerr == nil {
		return cur, nil
	}
	if errors.Is(gerr, ErrNotFound) {
		_, cerr := r.classify(ctx, owner, id)
		if cerr != nil && !errors.Is(cerr, ErrRevisionConflict) {
			return out, cerr
		}
	}
	return out, gerr
}

// Authenticate resolves a presented secret's hash to its live, unrevoked credential and stamps
// last_used_at in the same write, so a revocation is seen by the very next request. This is the
// one lookup that is not owner-scoped: the secret is what identifies the owner. secretHash must
// be the hash the service computed, never a client-supplied value.
func (r *Credentials) Authenticate(ctx context.Context, secretHash string) (IntegrationCredential, error) {
	var out IntegrationCredential
	if secretHash == "" {
		return out, ErrNotFound
	}
	filter := bson.D{{Key: "secret_hash", Value: secretHash}, {Key: "revoked_at", Value: nil}, {Key: "deleted_at", Value: nil}, {Key: "inactive_at", Value: nil}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "last_used_at", Value: r.now()}}}}
	err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
	if errors.Is(err, store.ErrNotFound) {
		return out, ErrNotFound
	}
	return out, err
}

// Revoke reasons the platform sets (PADR-0039).
const (
	ReasonOwnerDenied      = "owner_denied"
	ReasonOwnerRoleRemoved = "owner_role_removed"
)

// RevokeAllByOwner ends every active credential of owner, recording reason, and returns how many
// it ended. Each pass takes one credential, so a retry after a failure finishes the rest. This is
// the auth-event path, so it sets updated_by to the reason rather than a user.
func (r *Credentials) RevokeAllByOwner(ctx context.Context, owner, reason string) (int, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(reason) == "" {
		return 0, fmt.Errorf("%w: owner and reason are required", ErrInvalid)
	}
	n := 0
	for {
		now := r.now()
		update := bson.D{
			{Key: "$set", Value: bson.D{{Key: "revoked_at", Value: now}, {Key: "revoke_reason", Value: reason}, {Key: "updated_at", Value: now}, {Key: "updated_by", Value: reason}}},
			{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
		}
		var out IntegrationCredential
		err := r.c.FindOneAndUpdate(ctx, activeScope(owner), update, &out)
		if errors.Is(err, store.ErrNotFound) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

const gateID = "gate"

// GateClosed reports whether the persisted gate is closed. No document means open.
func (r *Credentials) GateClosed(ctx context.Context) (bool, error) {
	var doc struct {
		Closed bool `bson:"closed"`
	}
	err := r.gate.FindOne(ctx, bson.D{{Key: "_id", Value: gateID}}, &doc)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return doc.Closed, err
}

// SetGate persists the gate state so a restart cannot reopen it before a reconcile.
func (r *Credentials) SetGate(ctx context.Context, closed bool, actor string) error {
	col := r.gate
	filter := bson.D{{Key: "_id", Value: gateID}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "closed", Value: closed}, {Key: "updated_at", Value: r.now()}, {Key: "updated_by", Value: actor}}}}
	var out bson.M
	err := col.FindOneAndUpdate(ctx, filter, update, &out)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	err = col.InsertOne(ctx, bson.D{{Key: "_id", Value: gateID}, {Key: "closed", Value: closed}, {Key: "updated_at", Value: r.now()}, {Key: "updated_by", Value: actor}})
	if errors.Is(err, store.ErrDuplicate) {
		return col.FindOneAndUpdate(ctx, filter, update, &out)
	}
	return err
}
