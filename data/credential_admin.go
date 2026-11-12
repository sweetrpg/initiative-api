package data

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CredentialDeletion is the append-only record of an administrator's permanent deletion of an
// integration credential. Its _id is the deleted credential's id, so a retried deletion cannot
// write a second entry. Nothing in the service updates or removes these (PADR-0027).
type CredentialDeletion struct {
	CredentialID string    `bson:"_id" json:"credential_id"`
	OwnerUserID  string    `bson:"owner_user_id" json:"owner_user_id"`
	Label        string    `bson:"label" json:"label"`
	Scopes       []string  `bson:"scopes" json:"scopes"`
	Reason       string    `bson:"reason" json:"reason"`
	CreatedBy    string    `bson:"created_by" json:"deleted_by"`
	CreatedAt    time.Time `bson:"created_at" json:"deleted_at"`
}

// ownerPattern is the shape of a user id (Auth0 subject or users-api id). An owner filter taken
// from a request must match it, so only a plain id can reach a query.
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9_.|:-]{1,128}$`)

func checkOwner(owner string) error {
	if owner != "" && !ownerPattern.MatchString(owner) {
		return invalid("owner is not a valid user id")
	}
	return nil
}

// ListAll returns one page of credentials across owners, newest first. Administrator use only:
// callers gate it on the admin role. Its filter has no request input; one owner's credentials go
// through the owner-scoped List instead.
func (r *Credentials) ListAll(ctx context.Context, q Query) (Page[IntegrationCredential], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	}
	return runQuery[IntegrationCredential](ctx, r.c, r.spec, bson.D{{Key: "deleted_at", Value: nil}}, q)
}

// ListOwned is the owner-scoped List for an owner id taken from an administrator's request.
func (r *Credentials) ListOwned(ctx context.Context, owner string, q Query) (Page[IntegrationCredential], error) {
	if err := checkOwner(owner); err != nil {
		return Page[IntegrationCredential]{}, err
	}
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	}
	return r.List(ctx, owner, q)
}

// Live reports whether the credential can still authenticate: present, unrevoked, not over the
// owner's limit. Open streams re-check it so a revoke or deletion ends them.
func (r *Credentials) Live(ctx context.Context, id string) (bool, error) {
	id, ok := cleanID(id)
	if !ok {
		return false, nil
	}
	var c IntegrationCredential
	filter := bson.D{{Key: "_id", Value: id}, {Key: "revoked_at", Value: nil}, {Key: "deleted_at", Value: nil}, {Key: "inactive_at", Value: nil}}
	err := r.c.FindOne(ctx, filter, &c)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// DeletePermanently removes any owner's credential and records who did it and why. The audit
// entry is written first, so a failure after it leaves the credential in place and a retry
// completes the deletion without a second entry. Returns the entry.
func (r *Credentials) DeletePermanently(ctx context.Context, actor, id, reason string) (CredentialDeletion, error) {
	var out CredentialDeletion
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return out, fmt.Errorf("%w: administrator and reason are required", ErrInvalid)
	}
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	var c IntegrationCredential
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, &c)
	if errors.Is(err, store.ErrNotFound) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out = CredentialDeletion{
		CredentialID: c.ID, OwnerUserID: c.OwnerUserID, Label: c.Label, Scopes: c.Scopes,
		Reason: strings.TrimSpace(reason), CreatedBy: actor, CreatedAt: r.now(),
	}
	// A duplicate means an earlier attempt wrote the entry: return that stored row, so the
	// response and log match what the audit collection holds.
	if err := r.deletions.InsertOne(ctx, out); errors.Is(err, store.ErrDuplicate) {
		if err := r.deletions.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, &out); err != nil {
			return CredentialDeletion{}, err
		}
	} else if err != nil {
		return CredentialDeletion{}, err
	}
	if err := r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return CredentialDeletion{}, err
	}
	return out, nil
}

// DeletionSpec pages the audit entries, newest first.
var DeletionSpec = Spec{
	Name:     CollectionCredentialDeletions,
	Sortable: []string{"created_at"},
}

// Deletions returns one page of audit entries, newest first; a non-empty NextCursor means more.
// owner narrows it to one owner's deleted credentials.
func (r *Credentials) Deletions(ctx context.Context, owner string, q Query) (Page[CredentialDeletion], error) {
	if err := checkOwner(owner); err != nil {
		return Page[CredentialDeletion]{}, err
	}
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	}
	if owner == "" {
		return runQuery[CredentialDeletion](ctx, r.deletions, DeletionSpec, bson.D{}, q)
	}
	return runQuery[CredentialDeletion](ctx, r.deletions, DeletionSpec, bson.D{{Key: "owner_user_id", Value: owner}}, q)
}
