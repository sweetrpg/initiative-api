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
	// extra narrows every owner-scoped read and write beyond ownership, and exclude says which
	// stored documents that narrowing hides, so a miss is classified 403 instead of reading as a
	// revision conflict that would return the hidden document. Encounters use them to keep the
	// owner path to private encounters: a session encounter is reached only through its session
	// (PADR-0035), so an owner who left or was demoted cannot read or change it here.
	extra   bson.D
	exclude func(*T) bool
	// commit, when set, is told about every successful create, write, soft delete and flag change
	// with the stored document, so one place can announce them (see Encounters.SetSink).
	commit func(ctx context.Context, kind commitKind, before, after *T)
}

func (r *Owned[T, P]) committed(ctx context.Context, kind commitKind, before, after *T) {
	if r.commit != nil {
		r.commit(ctx, kind, before, after)
	}
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

// scoped is scope plus the repository's extra narrowing.
func (r *Owned[T, P]) scoped(owner string) bson.D {
	return append(scope(owner), r.extra...)
}

// scopedID is scopeID plus the repository's extra narrowing.
func (r *Owned[T, P]) scopedID(owner, id string) bson.D {
	return append(bson.D{{Key: "_id", Value: id}}, r.scoped(owner)...)
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
	return r.createWithID(ctx, ownerUserID, actingUserID, "", doc)
}

// createWithID is Create with a caller-chosen ID; an empty id means generate one.
func (r *Owned[T, P]) createWithID(ctx context.Context, ownerUserID, actingUserID, id string, doc P) error {
	if err := requireOwnerActor(ownerUserID, actingUserID); err != nil {
		return err
	}
	m := doc.GetMeta()
	m.ID = id
	if id == "" {
		m.ID = r.newID()
	}
	m.OwnerUserID = ownerUserID
	m.Revision = 1
	m.DeletedAt, m.DeletedBy = nil, nil
	models.StampCreate(&m.Auditable, actingUserID, r.now())
	if err := r.mapDuplicate(r.c.InsertOne(ctx, doc)); err != nil {
		return err
	}
	r.committed(ctx, commitCreate, nil, (*T)(doc))
	return nil
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
	err := r.c.FindOne(ctx, r.scopedID(ownerUserID, id), &out)
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
//
// Update applies set as given and checks nothing about its values. Size and name bounds on a
// roster's characters live in the domain layer (domain.Rosters), so a caller that reaches this
// method directly bypasses them; go through the domain.
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

// readBefore reads the document a revision-guarded write is about to change, for the event the
// commit emits. It is kept only if it is at the revision the write is made against: the write
// matches that revision or fails, so a kept document is exactly what the write changed. Anything
// else (no sink, a miss, a concurrent edit) is nil, and the event is built without a before.
func (r *Owned[T, P]) readBefore(ctx context.Context, owner, id string, expectedRevision int64) *T {
	if r.commit == nil {
		return nil
	}
	var b T
	if err := r.c.FindOne(ctx, r.scopedID(owner, id), &b); err != nil || P(&b).GetMeta().Revision != expectedRevision {
		return nil
	}
	return &b
}

func (r *Owned[T, P]) apply(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (T, error) {
	var out T
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	filter := append(r.scopedID(ownerUserID, id), bson.E{Key: "revision", Value: expectedRevision}, bson.E{Key: "inactive_at", Value: nil}, bson.E{Key: "locked_at", Value: nil})
	update := bson.D{
		{Key: "$set", Value: setDoc(set,
			bson.E{Key: "updated_at", Value: r.now()}, bson.E{Key: "updated_by", Value: actingUserID})},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	before := r.readBefore(ctx, ownerUserID, id, expectedRevision)
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
	if err = r.mapDuplicate(err); err == nil {
		r.committed(ctx, commitWrite, before, &out)
	}
	return out, err
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
	var after T
	filter := append(r.scopedID(ownerUserID, id), bson.E{Key: "revision", Value: expectedRevision})
	err := r.c.FindOneAndUpdate(ctx, filter, update, &after)
	if errors.Is(err, store.ErrNotFound) {
		_, err = r.classify(ctx, ownerUserID, id)
	}
	if err == nil {
		r.committed(ctx, commitDelete, nil, &after)
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
	case m.DeletedAt == nil && m.OwnerUserID == owner && r.exclude != nil && r.exclude(&cur):
		var zero T
		return zero, ErrForbidden
	case m.DeletedAt != nil:
		var zero T
		return zero, ErrNotFound
	case m.OwnerUserID != owner:
		var zero T
		return zero, ErrForbidden
	}
	return cur, ErrRevisionConflict
}
