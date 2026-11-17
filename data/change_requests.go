package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// Change request statuses.
const (
	ChangePending  = "pending"
	ChangeApproved = "approved"
	ChangeRejected = "rejected"
	// ChangeApproving is an approval's claim: the request is taken but the encounter write has not
	// finished. Views show it as pending. It becomes approved when the write succeeds, goes back to
	// pending if the write fails, and can be resumed by a GM once the claim is older than
	// ClaimTimeout.
	ChangeApproving = "approving"
	// ChangeCancelled marks a pending request whose requester left the session or lost their
	// player role before it was decided.
	ChangeCancelled = "cancelled"
)

// ClaimTimeout is how long an approving claim is left alone before another approval may resume it.
const ClaimTimeout = 60 * time.Second

// MaxPendingChangeRequests bounds one player's open requests in a session, so a client cannot
// grow the collection without limit while the GM is away.
const MaxPendingChangeRequests = 50

// maxCancelSweeps bounds how many requests one session deletion cancels in a pass: far more
// members than a session may have (20 plus the GM) times each one's pending cap. A session over
// it fails the deletion, which a retry continues.
const maxCancelSweeps = 100 * MaxPendingChangeRequests

// ChangeRequest is a player's proposed change to one combatant's health values or conditions
// (initiative-player-access). It changes nothing until a GM or co-GM approves it. Health holds the
// values to set (merged into the combatant's health, like a combatant patch) and is absolute, so
// applying it twice is harmless. Conditions are a delta by name, applied to the combatant as it
// is when the request is applied: RemoveConditions drops conditions by name, AddConditions sets
// (adds, or replaces the rounds of) a condition by name. A condition added by the GM after the
// request was submitted therefore survives approval.
type ChangeRequest struct {
	ID               string         `bson:"_id" json:"id"`
	SessionID        string         `bson:"session_id" json:"session_id"`
	EncounterID      string         `bson:"encounter_id" json:"encounter_id"`
	CombatantID      string         `bson:"combatant_id" json:"combatant_id"`
	RequestedBy      string         `bson:"requested_by" json:"requested_by"`
	Health           map[string]int `bson:"health,omitempty" json:"health,omitempty"`
	AddConditions    []Condition    `bson:"add_conditions,omitempty" json:"add_conditions,omitempty"`
	RemoveConditions []string       `bson:"remove_conditions,omitempty" json:"remove_conditions,omitempty"`
	Status           string         `bson:"status" json:"status"`
	// BaseRevision is the encounter revision the player saw when submitting.
	BaseRevision int64      `bson:"base_revision" json:"base_revision"`
	CreatedAt    time.Time  `bson:"created_at" json:"created_at"`
	DecidedBy    string     `bson:"decided_by,omitempty" json:"decided_by,omitempty"`
	DecidedAt    *time.Time `bson:"decided_at,omitempty" json:"decided_at,omitempty"`
	// Reason is why a rejected request was rejected.
	Reason string `bson:"reason,omitempty" json:"reason,omitempty"`
	// ClaimedAt is when an approval took the request (status approving).
	ClaimedAt *time.Time `bson:"claimed_at,omitempty" json:"-"`
	// AppliedRevision is the encounter revision an approved request produced.
	AppliedRevision int64 `bson:"applied_revision,omitempty" json:"applied_revision,omitempty"`
}

// MarshalJSON shows an approving request as pending: the claim is an internal step of an approval,
// and until it completes the change has not been applied.
func (r ChangeRequest) MarshalJSON() ([]byte, error) {
	type plain ChangeRequest
	if r.Status == ChangeApproving {
		r.Status, r.DecidedBy = ChangePending, ""
	}
	return json.Marshal(plain(r))
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
	// Count-then-insert: concurrent submissions can overrun the cap by at most the number of
	// requests in flight at once, which one player's connection count bounds.
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
	encounterID, ok := cleanID(encounterID)
	if !ok {
		return Page[ChangeRequest]{}, ErrNotFound
	}
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
	switch status {
	case "":
	case ChangePending:
		q.Filters = append(q.Filters, Filter{Field: "status", Op: OpIn, Values: []any{ChangePending, ChangeApproving}})
	default:
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
	encounterID, eok := cleanID(encounterID)
	if !ok || !eok {
		return req, ErrNotFound
	}
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID}}, &req)
	if errors.Is(err, store.ErrNotFound) {
		return req, ErrNotFound
	}
	return req, err
}

// Reject settles a pending request as rejected with reason, in one compare-and-set on status, so
// it cannot win against an approval that has claimed the request. A request in any other state is
// ErrConflict.
func (r *ChangeRequests) Reject(ctx context.Context, d Decision, encounterID, id, reason string) (ChangeRequest, error) {
	return r.settle(ctx, d, encounterID, id, bson.E{Key: "status", Value: ChangePending},
		bson.D{{Key: "status", Value: ChangeRejected}, {Key: "decided_by", Value: d.actor}, {Key: "decided_at", Value: r.now()}, {Key: "reason", Value: reason}})
}

// Claim takes a request for approval: pending becomes approving (claimed_at, decided_by), and an
// approving request whose claim is older than ClaimTimeout is taken over, so an approval that
// crashed can be resumed. A younger claim, and any decided request, is ErrConflict.
func (r *ChangeRequests) Claim(ctx context.Context, d Decision, encounterID, id string) (ChangeRequest, error) {
	now := r.now()
	open := bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: "status", Value: ChangePending}},
		bson.D{{Key: "status", Value: ChangeApproving}, {Key: "claimed_at", Value: bson.D{{Key: "$lte", Value: now.Add(-ClaimTimeout)}}}},
	}}
	return r.settle(ctx, d, encounterID, id, open,
		bson.D{{Key: "status", Value: ChangeApproving}, {Key: "decided_by", Value: d.actor}, {Key: "claimed_at", Value: now}})
}

// Finish records that the claimed approval's encounter write succeeded at revision: approving
// becomes approved. It only matches the claim d took; a request taken over since is ErrConflict
// (the change itself is idempotent, so the takeover's write does no harm).
func (r *ChangeRequests) Finish(ctx context.Context, d Decision, encounterID, id string, revision int64) (ChangeRequest, error) {
	return r.settle(ctx, d, encounterID, id, bson.E{Key: "$and", Value: bson.A{
		bson.D{{Key: "status", Value: ChangeApproving}}, bson.D{{Key: "decided_by", Value: d.actor}}}},
		bson.D{{Key: "status", Value: ChangeApproved}, {Key: "decided_at", Value: r.now()}, {Key: "applied_revision", Value: revision}})
}

// Unclaim puts a request this decision claimed back to pending, for an approval whose encounter
// write failed.
func (r *ChangeRequests) Unclaim(ctx context.Context, d Decision, encounterID, id string) error {
	if err := d.require(relEdit); err != nil {
		return err
	}
	id, ok := cleanID(id)
	encounterID, eok := cleanID(encounterID)
	if !ok || !eok {
		return ErrNotFound
	}
	var discard ChangeRequest
	err := r.c.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID},
			{Key: "status", Value: ChangeApproving}, {Key: "decided_by", Value: d.actor}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: ChangePending}}},
			{Key: "$unset", Value: bson.D{{Key: "decided_by", Value: ""}, {Key: "claimed_at", Value: ""}}}}, &discard)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// settle applies set to the request if it is in the state open describes, in one atomic write. When
// nothing matched, a request that exists is ErrConflict and one that does not is ErrNotFound.
func (r *ChangeRequests) settle(ctx context.Context, d Decision, encounterID, id string, open bson.E, set bson.D) (ChangeRequest, error) {
	var req ChangeRequest
	if err := d.require(relEdit); err != nil {
		return req, err
	}
	id, ok := cleanID(id)
	encounterID, eok := cleanID(encounterID)
	if !ok || !eok {
		return req, ErrNotFound
	}
	filter := bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "encounter_id", Value: encounterID}}
	err := r.c.FindOneAndUpdate(ctx, append(filter, open), bson.D{{Key: "$set", Value: set}}, &req)
	if !errors.Is(err, store.ErrNotFound) {
		return req, err
	}
	if ferr := r.c.FindOne(ctx, filter, &req); ferr == nil {
		return req, ErrConflict
	}
	return req, ErrNotFound
}

// CancelSession cancels every request still waiting (pending or approving) in the session, for a
// session that is being deleted. d must be the GM's. Idempotent; it fails rather than stop silently
// if more than maxCancelSweeps requests are waiting.
func (r *ChangeRequests) CancelSession(ctx context.Context, d Decision) error {
	if err := d.require(relGM); err != nil {
		return err
	}
	for range maxCancelSweeps {
		var discard ChangeRequest
		err := r.c.FindOneAndUpdate(ctx,
			bson.D{{Key: "session_id", Value: d.session}, {Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{ChangePending, ChangeApproving}}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: ChangeCancelled}, {Key: "decided_by", Value: d.actor}, {Key: "decided_at", Value: r.now()}}}}, &discard)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: more than %d change requests waiting in one session; retry delete", ErrInvalid, maxCancelSweeps)
}

// CancelPending cancels userID's pending and approving requests in the session, for a member who left, was
// removed, or lost the player role. d grants can_edit, or can_view for one's own leave. The
// per-player cap bounds the loop.
func (r *ChangeRequests) CancelPending(ctx context.Context, d Decision, userID string) error {
	need := relEdit
	if userID == d.actor {
		need = relView
	}
	if err := pairOK(d, need, userID); err != nil {
		return err
	}
	for range MaxPendingChangeRequests + 1 {
		var discard ChangeRequest
		err := r.c.FindOneAndUpdate(ctx,
			bson.D{{Key: "session_id", Value: d.session}, {Key: "requested_by", Value: userID},
				{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{ChangePending, ChangeApproving}}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: ChangeCancelled}, {Key: "decided_by", Value: d.actor}, {Key: "decided_at", Value: r.now()}}}}, &discard)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
