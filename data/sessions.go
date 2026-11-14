package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Session roles.
const (
	RoleGM       = "gm"
	RoleCoGM     = "co_gm"
	RolePlayer   = "player"
	RoleObserver = "observer"
)

// Join request states.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestRejected = "rejected"
)

// Session is a named group owned by one GM (the owner).
type Session struct {
	Meta `bson:",inline"`
	Name string `bson:"name" json:"name"`
	// OwnerLimits holds the owner's entitled values for the session-related keys as last read with
	// the owner's own token, and OwnerLimitsAt when. Members' checks use these because a member's
	// token cannot read the owner's entitlements (initiative-entitlement-gating).
	OwnerLimits   map[string]int `bson:"owner_limits,omitempty" json:"-"`
	OwnerLimitsAt *time.Time     `bson:"owner_limits_at,omitempty" json:"-"`
}

// Member is one user's place in a session. This collection is the source of truth for who holds
// which role (initiative-session-authorization).
type Member struct {
	ID        string    `bson:"_id" json:"-"`
	SessionID string    `bson:"session_id" json:"session_id"`
	UserID    string    `bson:"user_id" json:"user_id"`
	Role      string    `bson:"role" json:"role"`
	CreatedAt time.Time `bson:"created_at" json:"joined_at"`
	CreatedBy string    `bson:"created_by" json:"-"`
	// Live is set while the record is not deleted, so unique indexes can ignore deleted ones.
	Live      bool       `bson:"live,omitempty" json:"-"`
	DeletedAt *time.Time `bson:"deleted_at,omitempty" json:"-"`
	DeletedBy *string    `bson:"deleted_by,omitempty" json:"-"`
}

// Invite is a join code. Only its hash is stored, so the code is shown once, at creation.
type Invite struct {
	ID        string     `bson:"_id" json:"id"`
	SessionID string     `bson:"session_id" json:"session_id"`
	CodeHash  string     `bson:"code_hash" json:"-"`
	ExpiresAt time.Time  `bson:"expires_at" json:"expires_at"`
	RevokedAt *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	RevokedBy string     `bson:"revoked_by,omitempty" json:"revoked_by,omitempty"`
	CreatedAt time.Time  `bson:"created_at" json:"created_at"`
	CreatedBy string     `bson:"created_by" json:"created_by"`
	// Live is set while the record is not deleted, so unique indexes can ignore deleted ones.
	Live      bool       `bson:"live,omitempty" json:"-"`
	DeletedAt *time.Time `bson:"deleted_at,omitempty" json:"-"`
	DeletedBy *string    `bson:"deleted_by,omitempty" json:"-"`
}

// JoinRequest is a user's pending, approved, or rejected request to join.
type JoinRequest struct {
	ID        string     `bson:"_id" json:"id"`
	SessionID string     `bson:"session_id" json:"session_id"`
	UserID    string     `bson:"user_id" json:"user_id"`
	Status    string     `bson:"status" json:"status"`
	Role      string     `bson:"role,omitempty" json:"role,omitempty"`
	Reason    string     `bson:"reason,omitempty" json:"reason,omitempty"`
	DecidedBy string     `bson:"decided_by,omitempty" json:"decided_by,omitempty"`
	DecidedAt *time.Time `bson:"decided_at,omitempty" json:"decided_at,omitempty"`
	CreatedAt time.Time  `bson:"created_at" json:"created_at"`
	// Live is set while the record is not deleted, so unique indexes can ignore deleted ones.
	Live      bool       `bson:"live,omitempty" json:"-"`
	DeletedAt *time.Time `bson:"deleted_at,omitempty" json:"-"`
	DeletedBy *string    `bson:"deleted_by,omitempty" json:"-"`
}

// Block keeps a user from rejoining a session.
type Block struct {
	ID        string    `bson:"_id" json:"-"`
	SessionID string    `bson:"session_id" json:"session_id"`
	UserID    string    `bson:"user_id" json:"user_id"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	CreatedBy string    `bson:"created_by" json:"created_by"`
	// Live is set while the record is not deleted, so unique indexes can ignore deleted ones.
	Live      bool       `bson:"live,omitempty" json:"-"`
	DeletedAt *time.Time `bson:"deleted_at,omitempty" json:"-"`
	DeletedBy *string    `bson:"deleted_by,omitempty" json:"-"`
}

// Sessions is the repository for sessions and their membership records. The session document is
// owner-scoped (its owner is the GM); membership records are looked up by session, and the domain
// layer decides from them who may do what.
//
// The owner-scoped repository is not embedded: its Get, Update, List and the rest would be session
// reads and writes with no Decision (PADR-0035). Only Open, Count, SoftDelete and Limits expose it.
type Sessions struct {
	owned                              *Owned[Session, *Session]
	c                                  store.Collection
	members, invites, requests, blocks store.Collection
	now                                func() time.Time
}

// NewSessions builds the repository on db.
func NewSessions(db store.Database) *Sessions {
	o := NewOwned[Session](db, SessionSpec)
	return &Sessions{
		owned: o, c: o.c,
		members: db.Collection(CollectionSessionMembers), invites: db.Collection(CollectionSessionInvites),
		requests: db.Collection(CollectionSessionRequests), blocks: db.Collection(CollectionSessionBlocks),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func newID() string { return primitive.NewObjectID().Hex() }

// Every read of a membership collection filters deleted_at (PADR-0027: these records are soft
// deleted), so the two scope helpers carry it.
func sessionScope(sessionID string) bson.D {
	return bson.D{{Key: "session_id", Value: sessionID}, {Key: "deleted_at", Value: nil}}
}

func sessionUser(sessionID, userID string) bson.D {
	return bson.D{{Key: "session_id", Value: sessionID}, {Key: "user_id", Value: userID}, {Key: "deleted_at", Value: nil}}
}

// softDelete marks the first live record matching filter deleted by actor and returns ErrNotFound
// when there is none.
func (r *Sessions) softDelete(ctx context.Context, c store.Collection, filter bson.D, actor string) error {
	var gone bson.M
	err := c.FindOneAndUpdate(ctx, filter, bson.D{
		{Key: "$set", Value: bson.D{{Key: "deleted_at", Value: r.now()}, {Key: "deleted_by", Value: actor}}},
		{Key: "$unset", Value: bson.D{{Key: "live", Value: ""}}},
	}, &gone)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// pairOK validates the user id a call acts on.
func pairOK(d Decision, min, userID string) error {
	if err := d.require(min); err != nil {
		return err
	}
	return checkUserID(userID)
}

// checkUserID requires a plain user id, so an id from a request cannot reach a query as anything
// else.
func checkUserID(u string) error {
	if u == "" || !ownerPattern.MatchString(u) {
		return invalid("user id is not valid")
	}
	return nil
}

// Open creates the session owned by actor with the ID auth-api minted for them and records actor
// as its GM. Creating is not a session read: it is gated by the sessions_per_gm entitlement, which
// the caller checks first, and by the mint that put the GM tuple on id.
func (r *Sessions) Open(ctx context.Context, actor, id string, s *Session) error {
	id, ok := cleanID(id)
	if !ok {
		return invalid("session id is not valid")
	}
	if err := r.owned.createWithID(ctx, actor, actor, id, s); err != nil {
		return err
	}
	m := Member{ID: newID(), SessionID: s.ID, UserID: actor, Role: RoleGM, CreatedAt: r.now(), CreatedBy: actor, Live: true}
	if err := r.members.InsertOne(ctx, m); err != nil {
		// Without its GM the session could never be deleted, and it would count against
		// sessions_per_gm. Remove it; if even that fails, say so, since the caller ends the mint.
		if derr := r.owned.SoftDelete(ctx, actor, actor, s.ID, s.Revision); derr != nil {
			return fmt.Errorf("%w (and removing the session failed: %v)", err, derr)
		}
		return err
	}
	return nil
}

// Count returns how many sessions owner has, including inactive ones.
func (r *Sessions) Count(ctx context.Context, owner string) (int64, error) {
	return r.owned.Count(ctx, owner, nil)
}

// SoftDelete deletes the session at expectedRevision. Only the GM may.
func (r *Sessions) SoftDelete(ctx context.Context, d Decision, expectedRevision int64) error {
	if err := d.require(relGM); err != nil {
		return err
	}
	return r.owned.SoftDelete(ctx, d.actor, d.actor, d.session, expectedRevision)
}

// LimitItems is the part of the owner-scoped repository the limit reconciler needs: which sessions
// an owner has and which are inactive. It reads no session content.
type LimitItems struct{ o *Owned[Session, *Session] }

// Limits returns the limit reconciler's view of the sessions.
func (r *Sessions) Limits() LimitItems { return LimitItems{r.owned} }

// ItemStates lists owner's sessions with their activity state.
func (l LimitItems) ItemStates(ctx context.Context, owner string) ([]ItemState, error) {
	return l.o.ItemStates(ctx, owner)
}

// SetInactive sets or clears a session's inactive mark.
func (l LimitItems) SetInactive(ctx context.Context, owner, id string, inactive bool) error {
	return l.o.SetInactive(ctx, owner, id, inactive)
}

// SetKeepRanks records the owner's keep order.
func (l LimitItems) SetKeepRanks(ctx context.Context, owner string, ids []string) error {
	return l.o.SetKeepRanks(ctx, owner, ids)
}

// Live returns the session if it exists and is not deleted.
func (r *Sessions) Live(ctx context.Context, d Decision) (Session, error) {
	var s Session
	if err := d.require(relView); err != nil {
		return s, err
	}
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: d.session}, {Key: "deleted_at", Value: nil}}, &s)
	if errors.Is(err, store.ErrNotFound) {
		return s, ErrNotFound
	}
	return s, err
}

// AddMember records userID in the session with role. An existing member is ErrConflict.
func (r *Sessions) AddMember(ctx context.Context, d Decision, userID, role string) (Member, error) {
	if err := pairOK(d, relApprove, userID); err != nil {
		return Member{}, err
	}
	m := Member{ID: newID(), SessionID: d.session, UserID: userID, Role: role, CreatedAt: r.now(), CreatedBy: d.actor, Live: true}
	if err := r.members.InsertOne(ctx, m); errors.Is(err, store.ErrDuplicate) {
		return Member{}, ErrConflict
	} else if err != nil {
		return Member{}, err
	}
	return m, nil
}

// Member returns userID's membership, or ErrNotFound.
func (r *Sessions) Member(ctx context.Context, d Decision, userID string) (Member, error) {
	var m Member
	if err := pairOK(d, relView, userID); err != nil {
		return m, err
	}
	err := r.members.FindOne(ctx, sessionUser(d.session, userID), &m)
	if errors.Is(err, store.ErrNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

func byCreated(q Query) Query {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return q
}

// Members lists the session's members, oldest first.
func (r *Sessions) Members(ctx context.Context, d Decision, q Query) (Page[Member], error) {
	if err := d.require(relView); err != nil {
		return Page[Member]{}, err
	}
	return runQuery[Member](ctx, r.members, SessionMemberSpec, sessionScope(d.session), byCreated(q))
}

// SessionIDsOf lists the sessions userID belongs to. It is keyed by the caller's own membership
// records and returns no session data; the caller authorizes each session before reading it.
func (r *Sessions) SessionIDsOf(ctx context.Context, userID string, q Query) (Page[Member], error) {
	if err := checkUserID(userID); err != nil {
		return Page[Member]{}, err
	}
	return runQuery[Member](ctx, r.members, SessionMemberSpec, bson.D{{Key: "user_id", Value: userID}, {Key: "deleted_at", Value: nil}}, byCreated(q))
}

// RecordOwnerLimits stores the owner's current entitled values on the session. It changes neither
// the revision nor the audit stamp: nobody edited the session. Only the owner may: the write is
// owner-scoped to the decision's actor.
func (r *Sessions) RecordOwnerLimits(ctx context.Context, d Decision, limits map[string]int) error {
	if err := d.require(relView); err != nil {
		return err
	}
	var discard Session
	err := r.c.FindOneAndUpdate(ctx, scopeID(d.actor, d.session), bson.D{{Key: "$set", Value: bson.D{
		{Key: "owner_limits", Value: limits}, {Key: "owner_limits_at", Value: r.now()},
	}}}, &discard)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// SetRole changes a member's role.
func (r *Sessions) SetRole(ctx context.Context, d Decision, userID, role string) (Member, error) {
	var m Member
	if err := pairOK(d, relEdit, userID); err != nil {
		return m, err
	}
	err := r.members.FindOneAndUpdate(ctx, sessionUser(d.session, userID), bson.D{{Key: "$set", Value: bson.D{{Key: "role", Value: role}}}}, &m)
	if errors.Is(err, store.ErrNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

// RemoveMember ends a membership (soft delete).
func (r *Sessions) RemoveMember(ctx context.Context, d Decision, userID string) error {
	need := relEdit
	if userID == d.actor {
		need = relView // leaving needs only membership
	}
	if err := pairOK(d, need, userID); err != nil {
		return err
	}
	return r.softDelete(ctx, r.members, sessionUser(d.session, userID), d.actor)
}

// AddInvite stores an invite for the session.
func (r *Sessions) AddInvite(ctx context.Context, d Decision, codeHash string, expires time.Time) (Invite, error) {
	if err := d.require(relEdit); err != nil {
		return Invite{}, err
	}
	in := Invite{ID: newID(), SessionID: d.session, CodeHash: codeHash, ExpiresAt: expires, CreatedAt: r.now(), CreatedBy: d.actor, Live: true}
	return in, r.invites.InsertOne(ctx, in)
}

// JoinTicket is an invite the data layer has resolved and validated (known, live, not revoked, not
// expired) for one user. It is the gate for joining (initiative-sessions, "Join by code") and can
// only come from ResolveInvite, so the join calls cannot be aimed at an arbitrary session. The zero
// value is refused.
type JoinTicket struct{ actor, session string }

// ResolveInvite turns a presented code's hash into a ticket for actor, judging expiry at now. An unknown, deleted,
// revoked, or expired invite is ErrNotFound, whatever the reason.
func (r *Sessions) ResolveInvite(ctx context.Context, actor, codeHash string, now time.Time) (JoinTicket, error) {
	if err := checkUserID(actor); err != nil {
		return JoinTicket{}, err
	}
	if codeHash == "" {
		return JoinTicket{}, ErrNotFound
	}
	var in Invite
	err := r.invites.FindOne(ctx, bson.D{{Key: "code_hash", Value: codeHash}, {Key: "deleted_at", Value: nil}}, &in)
	if errors.Is(err, store.ErrNotFound) {
		return JoinTicket{}, ErrNotFound
	}
	if err != nil {
		return JoinTicket{}, err
	}
	if in.RevokedAt != nil || !now.Before(in.ExpiresAt) {
		return JoinTicket{}, ErrNotFound
	}
	return JoinTicket{actor: actor, session: in.SessionID}, nil
}

func (t JoinTicket) check() error {
	if t.actor == "" || t.session == "" {
		return ErrForbidden
	}
	return nil
}

// Invites lists the session's invites, oldest first.
func (r *Sessions) Invites(ctx context.Context, d Decision, q Query) (Page[Invite], error) {
	if err := d.require(relEdit); err != nil {
		return Page[Invite]{}, err
	}
	return runQuery[Invite](ctx, r.invites, SessionInviteSpec, sessionScope(d.session), byCreated(q))
}

// RevokeInvite ends an invite; revoking twice is harmless.
func (r *Sessions) RevokeInvite(ctx context.Context, d Decision, inviteID string) (Invite, error) {
	var in Invite
	if err := d.require(relEdit); err != nil {
		return in, err
	}
	id, ok := cleanID(inviteID)
	if !ok {
		return in, ErrNotFound
	}
	filter := append(bson.D{{Key: "_id", Value: id}}, sessionScope(d.session)...)
	err := r.invites.FindOne(ctx, filter, &in)
	if errors.Is(err, store.ErrNotFound) {
		return in, ErrNotFound
	}
	if err != nil || in.RevokedAt != nil {
		return in, err
	}
	err = r.invites.FindOneAndUpdate(ctx, append(filter, bson.E{Key: "revoked_at", Value: nil}),
		bson.D{{Key: "$set", Value: bson.D{{Key: "revoked_at", Value: r.now()}, {Key: "revoked_by", Value: d.actor}}}}, &in)
	if errors.Is(err, store.ErrNotFound) { // revoked concurrently
		return in, r.invites.FindOne(ctx, filter, &in)
	}
	return in, err
}

// JoinState is what a presented invite lets its holder learn: whether the session still exists
// and whether they are blocked or already a member. It carries no session data.
type JoinState struct {
	Exists, Active, Blocked, Member bool
}

// ForJoin reads the join state for the ticket holder. The ticket is the gate (initiative-sessions,
// "Join by code"), and the pending request created from it grants nothing.
func (r *Sessions) ForJoin(ctx context.Context, t JoinTicket) (JoinState, error) {
	var st JoinState
	if err := t.check(); err != nil {
		return st, err
	}
	actor := t.actor
	var sess Session
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: t.session}, {Key: "deleted_at", Value: nil}}, &sess)
	if errors.Is(err, store.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Exists, st.Active = true, sess.InactiveAt == nil
	var b Block
	switch err := r.blocks.FindOne(ctx, sessionUser(t.session, actor), &b); {
	case err == nil:
		st.Blocked = true
	case !errors.Is(err, store.ErrNotFound):
		return st, err
	}
	var m Member
	switch err := r.members.FindOne(ctx, sessionUser(t.session, actor), &m); {
	case err == nil:
		st.Member = true
	case !errors.Is(err, store.ErrNotFound):
		return st, err
	}
	return st, nil
}

// RequestJoin records a pending request from the ticket holder for the ticket's session. An earlier settled
// request becomes pending again (the caller has checked the user is not a member, so an approved
// one means the approval never finished); an existing pending one is returned unchanged.
func (r *Sessions) RequestJoin(ctx context.Context, t JoinTicket) (JoinRequest, error) {
	var jr JoinRequest
	if err := t.check(); err != nil {
		return jr, err
	}
	actor, sessionID := t.actor, t.session
	err := r.requests.FindOne(ctx, sessionUser(sessionID, actor), &jr)
	switch {
	case errors.Is(err, store.ErrNotFound):
		jr = JoinRequest{ID: newID(), SessionID: sessionID, UserID: actor, Status: RequestPending, CreatedAt: r.now(), Live: true}
		return jr, r.requests.InsertOne(ctx, jr)
	case err != nil:
		return jr, err
	case jr.Status == RequestPending:
		return jr, nil
	}
	// Decode into a fresh value: decoding over jr would keep the fields the update removed.
	var reset JoinRequest
	err = r.requests.FindOneAndUpdate(ctx, sessionUser(sessionID, actor), bson.D{
		{Key: "$set", Value: bson.D{{Key: "status", Value: RequestPending}, {Key: "created_at", Value: r.now()}}},
		{Key: "$unset", Value: bson.D{{Key: "reason", Value: ""}, {Key: "role", Value: ""}, {Key: "decided_by", Value: ""}, {Key: "decided_at", Value: ""}}},
	}, &reset)
	return reset, err
}

// Requests lists the session's requests, optionally only those with status.
func (r *Sessions) Requests(ctx context.Context, d Decision, status string, q Query) (Page[JoinRequest], error) {
	if err := d.require(relApprove); err != nil {
		return Page[JoinRequest]{}, err
	}
	q = byCreated(q)
	if status != "" {
		q.Filters = append(q.Filters, Filter{Field: "status", Op: OpEq, Values: []any{status}})
	}
	return runQuery[JoinRequest](ctx, r.requests, SessionRequestSpec, sessionScope(d.session), q)
}

// Decide settles a pending request as approved (with role) or rejected (with reason). A request
// already settled is ErrConflict.
func (r *Sessions) Decide(ctx context.Context, d Decision, requestID, status, role, reason string) (JoinRequest, error) {
	var jr JoinRequest
	if err := d.require(relApprove); err != nil {
		return jr, err
	}
	id, ok := cleanID(requestID)
	if !ok {
		return jr, ErrNotFound
	}
	filter := append(bson.D{{Key: "_id", Value: id}}, sessionScope(d.session)...)
	set := bson.D{{Key: "status", Value: status}, {Key: "decided_by", Value: d.actor}, {Key: "decided_at", Value: r.now()}}
	if role != "" {
		set = append(set, bson.E{Key: "role", Value: role})
	}
	if reason != "" {
		set = append(set, bson.E{Key: "reason", Value: reason})
	}
	err := r.requests.FindOneAndUpdate(ctx, append(filter, bson.E{Key: "status", Value: RequestPending}), bson.D{{Key: "$set", Value: set}}, &jr)
	if !errors.Is(err, store.ErrNotFound) {
		return jr, err
	}
	if ferr := r.requests.FindOne(ctx, filter, &jr); ferr == nil {
		return jr, ErrConflict
	}
	return jr, ErrNotFound
}

// DropRequest removes userID's request, if any.
func (r *Sessions) DropRequest(ctx context.Context, d Decision, userID string) error {
	if err := pairOK(d, relEdit, userID); err != nil {
		return err
	}
	if err := r.softDelete(ctx, r.requests, sessionUser(d.session, userID), d.actor); !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// AddBlock keeps userID out of the session; blocking twice is harmless.
func (r *Sessions) AddBlock(ctx context.Context, d Decision, userID string) error {
	if err := pairOK(d, relEdit, userID); err != nil {
		return err
	}
	err := r.blocks.InsertOne(ctx, Block{ID: newID(), SessionID: d.session, UserID: userID, CreatedAt: r.now(), CreatedBy: d.actor, Live: true})
	if errors.Is(err, store.ErrDuplicate) {
		return nil
	}
	return err
}

// RemoveBlock lifts a block.
func (r *Sessions) RemoveBlock(ctx context.Context, d Decision, userID string) error {
	if err := pairOK(d, relEdit, userID); err != nil {
		return err
	}
	return r.softDelete(ctx, r.blocks, sessionUser(d.session, userID), d.actor)
}

// Blocked reports whether userID is blocked from the session.
func (r *Sessions) Blocked(ctx context.Context, d Decision, userID string) (bool, error) {
	if err := pairOK(d, relEdit, userID); err != nil {
		return false, err
	}
	var b Block
	err := r.blocks.FindOne(ctx, sessionUser(d.session, userID), &b)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Blocks lists the session's blocked users.
func (r *Sessions) Blocks(ctx context.Context, d Decision, q Query) (Page[Block], error) {
	if err := d.require(relEdit); err != nil {
		return Page[Block]{}, err
	}
	return runQuery[Block](ctx, r.blocks, SessionBlockSpec, sessionScope(d.session), byCreated(q))
}

// DropSession soft-deletes every membership record of the session: members, invites, requests,
// and blocks. The session document itself is soft-deleted by SoftDelete.
func (r *Sessions) DropSession(ctx context.Context, d Decision) error {
	if err := d.require(relGM); err != nil {
		return err
	}
	return r.dropRecords(ctx, d.session, d.actor)
}

// dropRecords soft-deletes every membership record of the session as actor.
func (r *Sessions) dropRecords(ctx context.Context, sessionID, actor string) error {
	for _, c := range []store.Collection{r.members, r.invites, r.requests, r.blocks} {
		for {
			err := r.softDelete(ctx, c, sessionScope(sessionID), actor)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
