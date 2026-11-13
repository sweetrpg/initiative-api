package data

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/model-core.go/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Doc constrains P to a pointer to a document type that embeds Meta.
type Doc[T any] interface {
	*T
	GetMeta() *Meta
}

// Owned is the owner-scoped repository for one collection of user-owned documents. Every method
// requires the owner's user ID and folds it, plus the soft-delete filter, into the query itself.
// There is deliberately no method that reads or writes by ID alone.
type Owned[T any, P Doc[T]] struct {
	c     store.Collection
	spec  Spec
	now   func() time.Time
	newID func() string
}

// NewOwned builds the repository for spec's collection in db.
func NewOwned[T any, P Doc[T]](db store.Database, spec Spec) *Owned[T, P] {
	return &Owned[T, P]{
		c:     db.Collection(spec.Name),
		spec:  spec,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return primitive.NewObjectID().Hex() },
	}
}

// scope is the filter every owner-scoped query starts from.
func scope(owner string) bson.D {
	return bson.D{{Key: "owner_user_id", Value: owner}, {Key: "deleted_at", Value: nil}}
}

// scopeID is the owner-scoped filter for one record. id must already have passed cleanID.
func scopeID(owner, id string) bson.D {
	return append(bson.D{{Key: "_id", Value: id}}, scope(owner)...)
}

// cleanID returns the canonical form of a client-supplied record ID, or false when it is not a
// 24-character hex ObjectID. Every ID that reaches a query filter goes through here, so a request
// value can only ever be a plain ObjectID string, never an operator document or arbitrary text.
func cleanID(id string) (string, bool) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return "", false
	}
	return oid.Hex(), true
}

// requireOwnerActor refuses a write with no valid owner or acting user id.
func requireOwnerActor(owner, actor string) error {
	if err := checkUserID(owner); err != nil {
		return err
	}
	return checkUserID(actor)
}

// Create stores doc as owner's new record: it assigns the ID, owner, revision 1, and the audit
// stamp (created_by and updated_by are actor). Any ID or owner already on doc is overwritten.
func (r *Owned[T, P]) Create(ctx context.Context, ownerUserID, actingUserID string, doc P) error {
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return err
	}
	m := doc.GetMeta()
	m.ID = r.newID()
	m.OwnerUserID = ownerUserID
	m.Revision = 1
	m.DeletedAt, m.DeletedBy = nil, nil
	models.StampCreate(&m.Auditable, actingUserID, r.now())
	return r.mapDuplicate(r.c.InsertOne(ctx, doc))
}

// mapDuplicate turns a unique-index rejection into ErrConflict for specs that opt in.
func (r *Owned[T, P]) mapDuplicate(err error) error {
	if r.spec.ConflictOnDuplicate && errors.Is(err, store.ErrDuplicate) {
		return ErrConflict
	}
	return err
}

// Get returns owner's live record. Another owner's record is indistinguishable from a missing
// one on reads.
func (r *Owned[T, P]) Get(ctx context.Context, ownerUserID, id string) (T, error) {
	var out T
	if err := checkUserID(ownerUserID); err != nil {
		return out, err
	}
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	err := r.c.FindOne(ctx, scopeID(ownerUserID, id), &out)
	if errors.Is(err, store.ErrNotFound) {
		return out, ErrNotFound
	}
	return out, err
}

// reservedFields are managed by the data layer and rejected in Update's set.
var reservedFields = map[string]bool{
	"_id": true, "owner_user_id": true, "revision": true,
	"created_at": true, "created_by": true, "updated_at": true, "updated_by": true,
	"deleted_at": true, "deleted_by": true,
}

func validateSet(spec Spec, set map[string]any) error {
	if len(set) == 0 {
		return fmt.Errorf("%w: nothing to update", ErrInvalid)
	}
	for k := range set {
		if reservedFields[k] || strings.HasPrefix(k, "$") || strings.Contains(k, ".") || k == "" {
			return fmt.Errorf("%w: field %q cannot be set", ErrInvalid, k)
		}
		if !in(spec.Updatable, k) {
			return fmt.Errorf("%w: field %q is not updatable on %s", ErrInvalid, k, spec.Name)
		}
	}
	return nil
}

func setDoc(set map[string]any, extra ...bson.E) bson.D {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := make(bson.D, 0, len(keys)+len(extra))
	for _, k := range keys {
		d = append(d, bson.E{Key: k, Value: set[k]})
	}
	return append(d, extra...)
}

// Update sets fields on owner's record if its revision is still expectedRevision, bumping the
// revision and stamping updated_*. The match is one atomic write on {_id, owner, live, revision},
// so a stale revision or another owner's record modifies nothing. When nothing matched, the
// cause is classified afterwards: ErrNotFound, ErrForbidden, or ErrRevisionConflict (with the
// current record returned).
func (r *Owned[T, P]) Update(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (T, error) {
	var out T
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return out, err
	}
	if err := validateSet(r.spec, set); err != nil {
		return out, err
	}
	return r.apply(ctx, ownerUserID, actingUserID, id, expectedRevision, set)
}

// updateState is Update for the spec's StateFields: values computed by domain rules, never taken
// from client input. Everything else about the write is identical (owner-scoped, compare-and-set,
// classified failures).
func (r *Owned[T, P]) updateState(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (T, error) {
	var out T
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return out, err
	}
	if len(set) == 0 {
		return out, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}
	for k := range set {
		if !in(r.spec.StateFields, k) {
			return out, fmt.Errorf("%w: field %q is not a state field of %s", ErrInvalid, k, r.spec.Name)
		}
	}
	return r.apply(ctx, ownerUserID, actingUserID, id, expectedRevision, set)
}

// updateBoth writes settings (Updatable fields) and state (StateFields) in one atomic,
// compare-and-set write, for changes where a setting and the state derived from it must land
// together. Either map may be empty, not both.
func (r *Owned[T, P]) updateBoth(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, settings, state map[string]any) (T, error) {
	var out T
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return out, err
	}
	merged := make(map[string]any, len(settings)+len(state))
	if len(settings) > 0 {
		if err := validateSet(r.spec, settings); err != nil {
			return out, err
		}
		maps.Copy(merged, settings)
	}
	for k, v := range state {
		if !in(r.spec.StateFields, k) {
			return out, fmt.Errorf("%w: field %q is not a state field of %s", ErrInvalid, k, r.spec.Name)
		}
		merged[k] = v
	}
	if len(merged) == 0 {
		return out, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}
	return r.apply(ctx, ownerUserID, actingUserID, id, expectedRevision, merged)
}

func (r *Owned[T, P]) apply(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (T, error) {
	var out T
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	filter := append(scopeID(ownerUserID, id), bson.E{Key: "revision", Value: expectedRevision}, bson.E{Key: "inactive_at", Value: nil}, bson.E{Key: "locked_at", Value: nil})
	update := bson.D{
		{Key: "$set", Value: setDoc(set,
			bson.E{Key: "updated_at", Value: r.now()}, bson.E{Key: "updated_by", Value: actingUserID})},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
	if errors.Is(err, store.ErrNotFound) {
		cur, cerr := r.classify(ctx, ownerUserID, id)
		if errors.Is(cerr, ErrRevisionConflict) {
			var zero T
			switch m := P(&cur).GetMeta(); {
			case m.LockedAt != nil:
				return zero, ErrLocked
			case m.InactiveAt != nil:
				return zero, ErrInactive
			}
		}
		return cur, cerr
	}
	return out, r.mapDuplicate(err)
}

// SoftDelete tombstones owner's record if its revision is still expectedRevision (deleted_at/
// deleted_by, plus the spec's UnsetOnDelete fields), per PADR-0027 for user-owned domain data.
// Failures are classified like Update's, including ErrRevisionConflict.
func (r *Owned[T, P]) SoftDelete(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64) error {
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return err
	}
	now := r.now()
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "deleted_at", Value: now}, {Key: "deleted_by", Value: actingUserID},
			{Key: "updated_at", Value: now}, {Key: "updated_by", Value: actingUserID}}},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	if len(r.spec.UnsetOnDelete) > 0 {
		unset := make(bson.D, 0, len(r.spec.UnsetOnDelete))
		for _, f := range r.spec.UnsetOnDelete {
			unset = append(unset, bson.E{Key: f, Value: ""})
		}
		update = append(update, bson.E{Key: "$unset", Value: unset})
	}
	id, ok := cleanID(id)
	if !ok {
		return ErrNotFound
	}
	var discard T
	filter := append(scopeID(ownerUserID, id), bson.E{Key: "revision", Value: expectedRevision})
	err := r.c.FindOneAndUpdate(ctx, filter, update, &discard)
	if errors.Is(err, store.ErrNotFound) {
		_, err = r.classify(ctx, ownerUserID, id)
	}
	return err
}

// Missing explains why owner's scoped read of id found nothing: ErrNotFound when no live record
// exists, ErrForbidden when it belongs to another owner. Call it only after a scoped read or write
// has already failed; it never grants access. It returns nil if the record is visible to owner.
func (r *Owned[T, P]) Missing(ctx context.Context, owner, id string) error {
	if err := checkUserID(owner); err != nil {
		return err
	}
	if _, err := r.classify(ctx, owner, id); !errors.Is(err, ErrRevisionConflict) {
		return err
	}
	return nil
}

// classify runs only after an owner-scoped write matched nothing. It does an owner-agnostic
// lookup to tell 404 from 403 from 409; it never grants access.
func (r *Owned[T, P]) classify(ctx context.Context, owner, id string) (T, error) {
	var cur T
	id, ok := cleanID(id)
	if !ok {
		return cur, ErrNotFound
	}
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, &cur)
	if errors.Is(err, store.ErrNotFound) {
		return cur, ErrNotFound
	}
	if err != nil {
		return cur, err
	}
	m := P(&cur).GetMeta()
	switch {
	case m.DeletedAt != nil:
		var zero T
		return zero, ErrNotFound
	case m.OwnerUserID != owner:
		var zero T
		return zero, ErrForbidden
	}
	return cur, ErrRevisionConflict
}
