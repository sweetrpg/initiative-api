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
}

// NewCredentials builds the credential repository for db.
func NewCredentials(db store.Database) *Credentials {
	return &Credentials{NewOwned[IntegrationCredential](db, CredentialSpec)}
}

// activeScope is owner's credentials that are neither revoked nor deleted.
func activeScope(owner string) bson.D {
	return append(scope(owner), bson.E{Key: "revoked_at", Value: nil})
}

// CountActive counts owner's credentials that can still authenticate.
func (r *Credentials) CountActive(ctx context.Context, owner string) (int64, error) {
	if strings.TrimSpace(owner) == "" {
		return 0, fmt.Errorf("%w: owner is required", ErrInvalid)
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
	filter := bson.D{{Key: "secret_hash", Value: secretHash}, {Key: "revoked_at", Value: nil}, {Key: "deleted_at", Value: nil}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "last_used_at", Value: r.now()}}}}
	err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
	if errors.Is(err, store.ErrNotFound) {
		return out, ErrNotFound
	}
	return out, err
}
