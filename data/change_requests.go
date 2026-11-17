package data

import (
	"context"
	"errors"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// Change request statuses.
const (
	ChangePending  = "pending"
	ChangeApproved = "approved"
	ChangeRejected = "rejected"
)

// MaxPendingChangeRequests bounds one player's open requests in a session, so a client cannot
// grow the collection without limit while the GM is away.
const MaxPendingChangeRequests = 50

// ChangeRequest is a player's proposed change to one combatant's health values or conditions
// (initiative-player-access). It changes nothing until a GM or co-GM approves it. Health holds the
// values to set (merged into the combatant's health, like a combatant patch); Conditions, when
// present, replaces the combatant's conditions (an empty list clears them). The values are
// absolute, never deltas, so applying a request twice is harmless.
type ChangeRequest struct {
	ID          string         `bson:"_id" json:"id"`
	SessionID   string         `bson:"session_id" json:"session_id"`
	EncounterID string         `bson:"encounter_id" json:"encounter_id"`
	CombatantID string         `bson:"combatant_id" json:"combatant_id"`
	RequestedBy string         `bson:"requested_by" json:"requested_by"`
	Health      map[string]int `bson:"health,omitempty" json:"health,omitempty"`
	Conditions  *[]Condition   `bson:"conditions,omitempty" json:"conditions,omitempty"`
	Status      string         `bson:"status" json:"status"`
	// BaseRevision is the encounter revision the player saw when submitting.
	BaseRevision int64      `bson:"base_revision" json:"base_revision"`
	CreatedAt    time.Time  `bson:"created_at" json:"created_at"`
	DecidedBy    string     `bson:"decided_by,omitempty" json:"decided_by,omitempty"`
	DecidedAt    *time.Time `bson:"decided_at,omitempty" json:"decided_at,omitempty"`
	// Reason is why a rejected request was rejected.
	Reason string `bson:"reason,omitempty" json:"reason,omitempty"`
	// AppliedRevision is the encounter revision an approved request produced.
	AppliedRevision int64 `bson:"applied_revision,omitempty" json:"applied_revision,omitempty"`
}

// ChangeRequests is the repository for change requests. Every function takes the Viewer or
// Decision that proves the caller's standing in the session (PADR-0035) and scopes by it.
type ChangeRequests struct {
	c   store.Collection
	now func() time.Time
}

// NewChangeRequests builds the repository on db.
func NewChangeRequests(db store.Database) *ChangeRequests {
	return &ChangeRequests{c: db.Collection(CollectionChangeRequests), now: func() time.Time { return time.Now().UTC() }}
}

// Create stores req as the viewer's pending request. Only a player Viewer may: an observer has no
// change requests, and a GM edits directly. The session, requester, status and time are set here.
func (r *ChangeRequests) Create(ctx context.Context, v Viewer, req ChangeRequest) (ChangeRequest, error) {
	if v.level != ViewPlayer || v.session == "" || checkUserID(v.actor) != nil {
		return ChangeRequest{}, ErrForbidden
	}
	n, err := r.c.CountDocuments(ctx, bson.D{{Key: "session_id", Value: v.session}, {Key: "requested_by", Value: v.actor}, {Key: "status", Value: ChangePending}})
	if err != nil {
		return ChangeRequest{}, err
	}
	if n >= MaxPendingChangeRequests {
		return ChangeRequest{}, invalid("you have %d change requests waiting for the GM; wait for some to be decided", n)
	}
	req.ID, req.SessionID, req.RequestedBy = newID(), v.session, v.actor
	req.Status, req.CreatedAt = ChangePending, r.now()
	return req, r.c.InsertOne(ctx, req)
}

// List returns one page of the encounter's change requests, oldest first. A GM or co-GM sees all
// of them; a player only their own; an observer none.
func (r *ChangeRequests) List(ctx context.Context, v Viewer, encounterID, status string, q Query) (Page[ChangeRequest], error) {
	base := bson.D{{Key: "session_id", Value: v.session}, {Key: "encounter_id", Value: encounterID}}
	switch v.level {
	case ViewGM:
	case ViewPlayer:
		base = append(base, bson.E{Key: "requested_by", Value: v.actor})
	default:
		return Page[ChangeRequest]{}, ErrForbidden
	}
	if v.session == "" {
		return Page[ChangeRequest]{}, ErrForbidden
	}
	q = byCreated(q)
	if status != "" {
		q.Filters = append(q.Filters, Filter{Field: "status", Op: OpEq, Values: []any{status}})
	}
	return runQuery[ChangeRequest](ctx, r.c, ChangeRequestSpec, base, q)
}

// Load reads one change request of the decision's session and encounter. d must grant can_edit.
func (r *ChangeRequests) Load(ctx context.Context, d Decision, encounterID, id string) (ChangeRequest, error) {
	var req ChangeRequest
	if err := d.require(relEdit); err != nil {
		return req, err
	}
	id, ok := cleanID(id)
	if !ok {
		return req, ErrNotFound
	}
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID}}, &req)
	if errors.Is(err, store.ErrNotFound) {
		return req, ErrNotFound
	}
	return req, err
}

// Decide settles a pending request as approved or rejected (with reason) in one compare-and-set
// on status, so two deciders cannot both win. A request already decided is ErrConflict.
func (r *ChangeRequests) Decide(ctx context.Context, d Decision, encounterID, id, status, reason string) (ChangeRequest, error) {
	var req ChangeRequest
	if err := d.require(relEdit); err != nil {
		return req, err
	}
	id, ok := cleanID(id)
	if !ok {
		return req, ErrNotFound
	}
	filter := bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID}}
	set := bson.D{{Key: "status", Value: status}, {Key: "decided_by", Value: d.actor}, {Key: "decided_at", Value: r.now()}}
	if reason != "" {
		set = append(set, bson.E{Key: "reason", Value: reason})
	}
	err := r.c.FindOneAndUpdate(ctx, append(filter, bson.E{Key: "status", Value: ChangePending}), bson.D{{Key: "$set", Value: set}}, &req)
	if !errors.Is(err, store.ErrNotFound) {
		return req, err
	}
	if ferr := r.c.FindOne(ctx, filter, &req); ferr == nil {
		return req, ErrConflict
	}
	return req, ErrNotFound
}

// Reopen puts an approved request this decision approved back to pending, for an approval whose
// encounter write did not happen.
func (r *ChangeRequests) Reopen(ctx context.Context, d Decision, encounterID, id string) error {
	if err := d.require(relEdit); err != nil {
		return err
	}
	var discard ChangeRequest
	err := r.c.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID},
			{Key: "status", Value: ChangeApproved}, {Key: "decided_by", Value: d.actor}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: ChangePending}}},
			{Key: "$unset", Value: bson.D{{Key: "decided_by", Value: ""}, {Key: "decided_at", Value: ""}}}}, &discard)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// MarkApplied records the encounter revision an approved request produced.
func (r *ChangeRequests) MarkApplied(ctx context.Context, d Decision, encounterID, id string, revision int64) (ChangeRequest, error) {
	var req ChangeRequest
	if err := d.require(relEdit); err != nil {
		return req, err
	}
	err := r.c.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID}, {Key: "status", Value: ChangeApproved}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "applied_revision", Value: revision}}}}, &req)
	if errors.Is(err, store.ErrNotFound) {
		return req, ErrNotFound
	}
	return req, err
}
