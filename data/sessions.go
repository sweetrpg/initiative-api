package data

import (
	"context"
	"errors"
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
type Sessions struct {
	*Owned[Session, *Session]
	members, invites, requests, blocks store.Collection
	now                                func() time.Time
}

// NewSessions builds the repository on db.
func NewSessions(db store.Database) *Sessions {
	return &Sessions{
		Owned:   NewOwned[Session](db, SessionSpec),
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

func (r *Sessions) pairFilterOK(sessionID, userID string) error {
	if _, ok := cleanID(sessionID); !ok {
		return ErrNotFound
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

// Live returns the session if it exists and is not deleted. It does not check access.
func (r *Sessions) Live(ctx context.Context, sessionID string) (Session, error) {
	var s Session
	id, ok := cleanID(sessionID)
	if !ok {
		return s, ErrNotFound
	}
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "deleted_at", Value: nil}}, &s)
	if errors.Is(err, store.ErrNotFound) {
		return s, ErrNotFound
	}
	return s, err
}

// AddMember records userID in the session with role. An existing member is ErrConflict.
func (r *Sessions) AddMember(ctx context.Context, sessionID, userID, role, actor string) (Member, error) {
	m := Member{ID: newID(), SessionID: sessionID, UserID: userID, Role: role, CreatedAt: r.now(), CreatedBy: actor, Live: true}
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return m, err
	}
	if err := r.members.InsertOne(ctx, m); errors.Is(err, store.ErrDuplicate) {
		return Member{}, ErrConflict
	} else if err != nil {
		return Member{}, err
	}
	return m, nil
}

// Member returns userID's membership, or ErrNotFound.
func (r *Sessions) Member(ctx context.Context, sessionID, userID string) (Member, error) {
	var m Member
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return m, err
	}
	err := r.members.FindOne(ctx, sessionUser(sessionID, userID), &m)
	if errors.Is(err, store.ErrNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

// Members lists the session's members, oldest first.
func (r *Sessions) Members(ctx context.Context, sessionID string, q Query) (Page[Member], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return runQuery[Member](ctx, r.members, SessionMemberSpec, sessionScope(sessionID), q)
}

// SessionIDsOf lists the sessions userID belongs to.
func (r *Sessions) SessionIDsOf(ctx context.Context, userID string, q Query) (Page[Member], error) {
	if err := checkUserID(userID); err != nil {
		return Page[Member]{}, err
	}
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return runQuery[Member](ctx, r.members, SessionMemberSpec, bson.D{{Key: "user_id", Value: userID}}, q)
}

// SetRole changes a member's role.
func (r *Sessions) SetRole(ctx context.Context, sessionID, userID, role string) (Member, error) {
	var m Member
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return m, err
	}
	err := r.members.FindOneAndUpdate(ctx, sessionUser(sessionID, userID), bson.D{{Key: "$set", Value: bson.D{{Key: "role", Value: role}}}}, &m)
	if errors.Is(err, store.ErrNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

// RemoveMember ends a membership (soft delete).
func (r *Sessions) RemoveMember(ctx context.Context, sessionID, userID, actor string) error {
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return err
	}
	return r.softDelete(ctx, r.members, sessionUser(sessionID, userID), actor)
}

// AddInvite stores an invite for the session.
func (r *Sessions) AddInvite(ctx context.Context, sessionID, codeHash string, expires time.Time, actor string) (Invite, error) {
	in := Invite{ID: newID(), SessionID: sessionID, CodeHash: codeHash, ExpiresAt: expires, CreatedAt: r.now(), CreatedBy: actor, Live: true}
	return in, r.invites.InsertOne(ctx, in)
}

// InviteByHash resolves a presented code's hash to its invite, whatever its state.
func (r *Sessions) InviteByHash(ctx context.Context, codeHash string) (Invite, error) {
	var in Invite
	if codeHash == "" {
		return in, ErrNotFound
	}
	err := r.invites.FindOne(ctx, bson.D{{Key: "code_hash", Value: codeHash}, {Key: "deleted_at", Value: nil}}, &in)
	if errors.Is(err, store.ErrNotFound) {
		return in, ErrNotFound
	}
	return in, err
}

// Invites lists the session's invites, oldest first.
func (r *Sessions) Invites(ctx context.Context, sessionID string, q Query) (Page[Invite], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return runQuery[Invite](ctx, r.invites, SessionInviteSpec, sessionScope(sessionID), q)
}

// RevokeInvite ends an invite; revoking twice is harmless.
func (r *Sessions) RevokeInvite(ctx context.Context, sessionID, inviteID string) (Invite, error) {
	var in Invite
	id, ok := cleanID(inviteID)
	if !ok {
		return in, ErrNotFound
	}
	filter := append(bson.D{{Key: "_id", Value: id}}, sessionScope(sessionID)...)
	err := r.invites.FindOne(ctx, filter, &in)
	if errors.Is(err, store.ErrNotFound) {
		return in, ErrNotFound
	}
	if err != nil || in.RevokedAt != nil {
		return in, err
	}
	err = r.invites.FindOneAndUpdate(ctx, append(filter, bson.E{Key: "revoked_at", Value: nil}),
		bson.D{{Key: "$set", Value: bson.D{{Key: "revoked_at", Value: r.now()}}}}, &in)
	if errors.Is(err, store.ErrNotFound) { // revoked concurrently
		return in, r.invites.FindOne(ctx, filter, &in)
	}
	return in, err
}

// RequestJoin records a pending request from userID. An earlier settled request becomes pending
// again (the caller has checked the user is not a member, so an approved one means the approval
// never finished); an existing pending one is returned unchanged.
func (r *Sessions) RequestJoin(ctx context.Context, sessionID, userID string) (JoinRequest, error) {
	var jr JoinRequest
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return jr, err
	}
	err := r.requests.FindOne(ctx, sessionUser(sessionID, userID), &jr)
	switch {
	case errors.Is(err, store.ErrNotFound):
		jr = JoinRequest{ID: newID(), SessionID: sessionID, UserID: userID, Status: RequestPending, CreatedAt: r.now(), Live: true}
		return jr, r.requests.InsertOne(ctx, jr)
	case err != nil:
		return jr, err
	case jr.Status == RequestPending:
		return jr, nil
	}
	// Decode into a fresh value: decoding over jr would keep the fields the update removed.
	var reset JoinRequest
	err = r.requests.FindOneAndUpdate(ctx, sessionUser(sessionID, userID), bson.D{
		{Key: "$set", Value: bson.D{{Key: "status", Value: RequestPending}, {Key: "created_at", Value: r.now()}}},
		{Key: "$unset", Value: bson.D{{Key: "reason", Value: ""}, {Key: "role", Value: ""}, {Key: "decided_by", Value: ""}, {Key: "decided_at", Value: ""}}},
	}, &reset)
	return reset, err
}

// Requests lists the session's requests, optionally only those with status.
func (r *Sessions) Requests(ctx context.Context, sessionID, status string, q Query) (Page[JoinRequest], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	if status != "" {
		q.Filters = append(q.Filters, Filter{Field: "status", Op: OpEq, Values: []any{status}})
	}
	return runQuery[JoinRequest](ctx, r.requests, SessionRequestSpec, sessionScope(sessionID), q)
}

// Decide settles a pending request as approved (with role) or rejected (with reason). A request
// already settled is ErrConflict.
func (r *Sessions) Decide(ctx context.Context, sessionID, requestID, status, role, reason, actor string) (JoinRequest, error) {
	var jr JoinRequest
	id, ok := cleanID(requestID)
	if !ok {
		return jr, ErrNotFound
	}
	filter := append(bson.D{{Key: "_id", Value: id}}, sessionScope(sessionID)...)
	set := bson.D{{Key: "status", Value: status}, {Key: "decided_by", Value: actor}, {Key: "decided_at", Value: r.now()}}
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
func (r *Sessions) DropRequest(ctx context.Context, sessionID, userID, actor string) error {
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return err
	}
	if err := r.softDelete(ctx, r.requests, sessionUser(sessionID, userID), actor); !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// AddBlock keeps userID out of the session; blocking twice is harmless.
func (r *Sessions) AddBlock(ctx context.Context, sessionID, userID, actor string) error {
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return err
	}
	err := r.blocks.InsertOne(ctx, Block{ID: newID(), SessionID: sessionID, UserID: userID, CreatedAt: r.now(), CreatedBy: actor, Live: true})
	if errors.Is(err, store.ErrDuplicate) {
		return nil
	}
	return err
}

// RemoveBlock lifts a block.
func (r *Sessions) RemoveBlock(ctx context.Context, sessionID, userID, actor string) error {
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return err
	}
	return r.softDelete(ctx, r.blocks, sessionUser(sessionID, userID), actor)
}

// Blocked reports whether userID is blocked from the session.
func (r *Sessions) Blocked(ctx context.Context, sessionID, userID string) (bool, error) {
	if err := r.pairFilterOK(sessionID, userID); err != nil {
		return false, err
	}
	var b Block
	err := r.blocks.FindOne(ctx, sessionUser(sessionID, userID), &b)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Blocks lists the session's blocked users.
func (r *Sessions) Blocks(ctx context.Context, sessionID string, q Query) (Page[Block], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return runQuery[Block](ctx, r.blocks, SessionBlockSpec, sessionScope(sessionID), q)
}

// DropSession soft-deletes every membership record of the session: members, invites, requests,
// and blocks. The session document itself is soft-deleted by SoftDelete.
func (r *Sessions) DropSession(ctx context.Context, sessionID, actor string) error {
	if _, ok := cleanID(sessionID); !ok {
		return ErrNotFound
	}
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
