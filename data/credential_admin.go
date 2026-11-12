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

// ListAll returns one page of credentials across owners, newest first. owner narrows it to one
// owner when set. Administrator use only: callers gate it on the admin role.
func (r *Credentials) ListAll(ctx context.Context, owner string, q Query) (Page[IntegrationCredential], error) {
	if err := checkOwner(owner); err != nil {
		return Page[IntegrationCredential]{}, err
	}
	base := bson.D{{Key: "deleted_at", Value: nil}}
	if owner != "" {
		base = append(base, bson.E{Key: "owner_user_id", Value: owner})
	}
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	}
	return runQuery[IntegrationCredential](ctx, r.c, r.spec, base, q)
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
	if err := r.deletions.InsertOne(ctx, out); err != nil && !errors.Is(err, store.ErrDuplicate) {
		return CredentialDeletion{}, err
	}
	if err := r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return CredentialDeletion{}, err
	}
	return out, nil
}

// Deletions lists the audit entries for credentials deleted from owner's account (all owners
// when owner is empty), newest first.
func (r *Credentials) Deletions(ctx context.Context, owner string) ([]CredentialDeletion, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	filter := bson.D{}
	if owner != "" {
		filter = bson.D{{Key: "owner_user_id", Value: owner}}
	}
	var out []CredentialDeletion
	err := r.deletions.Find(ctx, filter, store.FindOptions{Sort: bson.D{{Key: "created_at", Value: -1}}, Limit: 200}, &out)
	return out, err
}
