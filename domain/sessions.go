package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store"
)

// Action is something a session member may try to do. Allowed is the one place the role rules
// live (spec initiative-sessions, "Roles").
type Action string

// Session actions.
const (
	ActView          Action = "view"
	ActInvite        Action = "invite"
	ActDecideRequest Action = "decide_request"
	ActManageMembers Action = "manage_members"
	ActBlock         Action = "block"
	ActLeave         Action = "leave"
	ActDelete        Action = "delete_session"
)

var allowed = map[Action][]string{
	ActView:          {data.RoleGM, data.RoleCoGM, data.RolePlayer, data.RoleObserver},
	ActInvite:        {data.RoleGM, data.RoleCoGM},
	ActDecideRequest: {data.RoleGM, data.RoleCoGM},
	ActManageMembers: {data.RoleGM, data.RoleCoGM},
	ActBlock:         {data.RoleGM, data.RoleCoGM},
	ActLeave:         {data.RoleCoGM, data.RolePlayer, data.RoleObserver},
	ActDelete:        {data.RoleGM},
}

// Allowed reports whether a member holding role may take action.
func Allowed(role string, a Action) bool { return slices.Contains(allowed[a], role) }

// assignable are the roles a manager can give. There is exactly one gm, so gm is never assigned.
var assignable = []string{data.RoleCoGM, data.RolePlayer, data.RoleObserver}

// Invite lifetimes.
const (
	defaultInviteTTL = 7 * 24 * time.Hour
	minInviteTTL     = time.Hour
	maxInviteTTL     = 30 * 24 * time.Hour
	maxSessionName   = 100
)

// ErrInviteInvalid covers an unknown, expired, revoked, or blocked invite alike, so a code reveals
// nothing about why it failed.
var ErrInviteInvalid = fmt.Errorf("%w: invite is not valid", data.ErrInvalid)

// ErrOwnerLimitsUnknown means the session has no recorded owner entitlements yet and the owner is
// not the caller. It is retryable: the GM's next request records them.
var ErrOwnerLimitsUnknown = errors.New("domain: session owner entitlements not recorded yet")

// ownerLimitsRefreshEvery bounds how often an unchanged record is rewritten.
const ownerLimitsRefreshEvery = time.Minute

// sessionKeys are the owner entitlements recorded on a session.
var sessionKeys = []string{KeyMembersPerSession}

// OwnerLimit returns the owner's recorded value for key, or ErrOwnerLimitsUnknown if none yet.
func OwnerLimit(sess data.Session, key string) (int, error) {
	v, ok := sess.OwnerLimits[key]
	if !ok {
		return 0, ErrOwnerLimitsUnknown
	}
	return v, nil
}

// SessionView is a session with the caller's role in it.
type SessionView struct {
	data.Session
	Role string `json:"role"`
}

// Sessions implements session membership and its rules.
type Sessions struct {
	repo *data.Sessions
	now  func() time.Time
}

// NewSessions builds the service on db.
func NewSessions(db store.Database) *Sessions {
	return &Sessions{repo: data.NewSessions(db), now: func() time.Time { return time.Now().UTC() }}
}

// Create makes actor the GM of a new session.
// Sessions beyond the GM's entitled count are refused with *EntitlementError; inactive sessions
// count, so a GM over a reduced limit cannot create.
func (s *Sessions) Create(ctx context.Context, actor, name string) (SessionView, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxSessionName {
		return SessionView{}, invalid("session name is required and at most %d characters", maxSessionName)
	}
	chk, ok := ctx.Value(checkerKey{}).(Checker)
	if !ok {
		return SessionView{}, entitlements.ErrUnavailable
	}
	res, err := chk.Check(ctx, KeySessionsPerGM, entitlements.FailClosed)
	if err != nil {
		return SessionView{}, err
	}
	owned, err := s.repo.Count(ctx, actor, nil)
	if err != nil {
		return SessionView{}, err
	}
	if owned >= int64(res.Limit) {
		return SessionView{}, &EntitlementError{Key: KeySessionsPerGM, Limit: res.Limit}
	}
	sess := data.Session{Name: name}
	if err := s.repo.Open(ctx, actor, &sess); err != nil {
		return SessionView{}, err
	}
	return SessionView{Session: sess, Role: data.RoleGM}, nil
}

// actionRelation is the OpenFGA relation on the session each action needs. The role rules in
// Allowed still apply on top: OpenFGA says whether the caller may touch the session at all.
var actionRelation = map[Action]string{
	ActView: "can_view", ActLeave: "can_view",
	ActInvite: "can_edit", ActBlock: "can_edit", ActManageMembers: "can_edit",
	ActDecideRequest: "can_approve",
	ActDelete:        "gm",
}

// as authorizes actor for action a on session id with auth-api (OpenFGA), then returns the
// session, actor's membership, and the decision the data layer needs. A denial is ErrForbidden and
// reads nothing; an unavailable decision is relations.ErrUnavailable (503), never an allow.
func (s *Sessions) as(ctx context.Context, actor, id string, a Action) (data.Session, data.Member, data.Decision, error) {
	d, err := data.Authorize(ctx, actor, id, actionRelation[a])
	if err != nil {
		return data.Session{}, data.Member{}, d, err
	}
	sess, err := s.repo.Live(ctx, d)
	if err != nil {
		return sess, data.Member{}, d, err
	}
	m, err := s.repo.Member(ctx, d, actor)
	if errors.Is(err, data.ErrNotFound) || (err == nil && !Allowed(m.Role, a)) {
		return data.Session{}, data.Member{}, d, data.ErrForbidden
	}
	if err != nil {
		return data.Session{}, data.Member{}, d, err
	}
	if m.Role == data.RoleGM {
		sess = s.refreshOwnerLimits(ctx, d, sess)
	}
	if sess.InactiveAt != nil && a != ActView && a != ActDelete && a != ActLeave {
		return sess, m, d, data.ErrInactive
	}
	return sess, m, d, nil
}

// refreshOwnerLimits re-reads the GM's entitlements with the GM's own token (the checker in ctx)
// and records them on the session. A failed, degraded, or stale (cached past its TTL) read keeps the old record, is logged, and
// never fails the request.
func (s *Sessions) refreshOwnerLimits(ctx context.Context, d data.Decision, sess data.Session) data.Session {
	chk, ok := ctx.Value(checkerKey{}).(Checker)
	if !ok {
		return sess
	}
	limits := make(map[string]int, len(sessionKeys))
	for _, key := range sessionKeys {
		res, err := chk.Check(ctx, key, entitlements.FailOpen)
		if err != nil || res.Degraded || res.Stale {
			slog.WarnContext(ctx, "session owner entitlements not refreshed", "session", sess.ID, "key", key, "error", err)
			return sess
		}
		limits[key] = res.Limit
	}
	fresh := sess.OwnerLimitsAt != nil && s.now().Sub(*sess.OwnerLimitsAt) < ownerLimitsRefreshEvery
	if fresh && maps.Equal(limits, sess.OwnerLimits) {
		return sess
	}
	if err := s.repo.RecordOwnerLimits(ctx, d, limits); err != nil {
		slog.WarnContext(ctx, "session owner entitlements not recorded", "session", sess.ID, "error", err)
		return sess
	}
	now := s.now()
	sess.OwnerLimits, sess.OwnerLimitsAt = limits, &now
	return sess
}

// Items exposes the sessions as limited items, so the limit reconciler can deactivate those beyond
// the owner's entitled count.
func (s *Sessions) Items() Limited { return s.repo }

// Get returns the session with actor's role. Only members may read it.
func (s *Sessions) Get(ctx context.Context, actor, id string) (SessionView, error) {
	sess, m, _, err := s.as(ctx, actor, id, ActView)
	return SessionView{Session: sess, Role: m.Role}, err
}

// List returns the sessions actor belongs to.
func (s *Sessions) List(ctx context.Context, actor string, q data.Query) (data.Page[SessionView], error) {
	page, err := s.repo.SessionIDsOf(ctx, actor, q)
	if err != nil {
		return data.Page[SessionView]{}, err
	}
	out := data.Page[SessionView]{NextCursor: page.NextCursor, Items: []SessionView{}}
	for _, m := range page.Items {
		d, err := data.Authorize(ctx, actor, m.SessionID, actionRelation[ActView])
		if errors.Is(err, data.ErrForbidden) || errors.Is(err, data.ErrNotFound) {
			continue // membership record the tuples don't back (yet), or malformed
		}
		if err != nil {
			return data.Page[SessionView]{}, err
		}
		sess, err := s.repo.Live(ctx, d)
		if errors.Is(err, data.ErrNotFound) {
			continue // deleted while we listed
		}
		if err != nil {
			return data.Page[SessionView]{}, err
		}
		out.Items = append(out.Items, SessionView{Session: sess, Role: m.Role})
	}
	return out, nil
}

// Delete soft-deletes the session and ends every membership. Only the GM may; the owner-scoped
// delete enforces it a second time.
func (s *Sessions) Delete(ctx context.Context, actor, id string, rev int64) error {
	_, _, d, err := s.as(ctx, actor, id, ActDelete)
	if err != nil {
		return err
	}
	if err := s.repo.SoftDelete(ctx, actor, actor, d.SessionID(), rev); err != nil {
		return err
	}
	return s.repo.DropSession(ctx, d)
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

func newCode() (string, error) {
	b := make([]byte, 10) // 80 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// CreateInvite makes a join code for the session. The code is returned once; only its hash is kept.
// ttl 0 means seven days.
func (s *Sessions) CreateInvite(ctx context.Context, actor, id string, ttl time.Duration) (data.Invite, string, error) {
	_, _, d, err := s.as(ctx, actor, id, ActInvite)
	if err != nil {
		return data.Invite{}, "", err
	}
	if ttl == 0 {
		ttl = defaultInviteTTL
	}
	if ttl < minInviteTTL || ttl > maxInviteTTL {
		return data.Invite{}, "", invalid("invite lifetime must be between %s and %s", minInviteTTL, maxInviteTTL)
	}
	code, err := newCode()
	if err != nil {
		return data.Invite{}, "", err
	}
	in, err := s.repo.AddInvite(ctx, d, hashCode(code), s.now().Add(ttl))
	return in, code, err
}

// Invites lists the session's invites.
func (s *Sessions) Invites(ctx context.Context, actor, id string, q data.Query) (data.Page[data.Invite], error) {
	_, _, d, err := s.as(ctx, actor, id, ActInvite)
	if err != nil {
		return data.Page[data.Invite]{}, err
	}
	return s.repo.Invites(ctx, d, q)
}

// RevokeInvite ends an invite at once.
func (s *Sessions) RevokeInvite(ctx context.Context, actor, id, inviteID string) (data.Invite, error) {
	_, _, d, err := s.as(ctx, actor, id, ActInvite)
	if err != nil {
		return data.Invite{}, err
	}
	return s.repo.RevokeInvite(ctx, d, inviteID)
}

// Join turns a presented code into a pending request. A pending request grants nothing. A code
// that is unknown, expired, revoked, or from a blocked user fails the same way and creates nothing.
func (s *Sessions) Join(ctx context.Context, actor, code string) (data.JoinRequest, error) {
	t, err := s.repo.ResolveInvite(ctx, actor, hashCode(code), s.now())
	if errors.Is(err, data.ErrNotFound) {
		return data.JoinRequest{}, ErrInviteInvalid
	}
	if err != nil {
		return data.JoinRequest{}, err
	}
	st, err := s.repo.ForJoin(ctx, t)
	if err != nil {
		return data.JoinRequest{}, err
	}
	if !st.Exists || !st.Active || st.Blocked {
		return data.JoinRequest{}, ErrInviteInvalid
	}
	if st.Member {
		return data.JoinRequest{}, data.ErrConflict
	}
	return s.repo.RequestJoin(ctx, t)
}

// Requests lists the session's join requests (pending ones when pendingOnly).
func (s *Sessions) Requests(ctx context.Context, actor, id string, pendingOnly bool, q data.Query) (data.Page[data.JoinRequest], error) {
	_, _, d, err := s.as(ctx, actor, id, ActDecideRequest)
	if err != nil {
		return data.Page[data.JoinRequest]{}, err
	}
	status := ""
	if pendingOnly {
		status = data.RequestPending
	}
	return s.repo.Requests(ctx, d, status, q)
}

// Approve makes the requester a member with role. The request settles first, so two approvals
// cannot both succeed; a block placed after the request still stops it.
func (s *Sessions) Approve(ctx context.Context, actor, id, requestID, role string) (data.Member, error) {
	if !slices.Contains(assignable, role) {
		return data.Member{}, invalid("role must be one of %s", strings.Join(assignable, ", "))
	}
	_, _, d, err := s.as(ctx, actor, id, ActDecideRequest)
	if err != nil {
		return data.Member{}, err
	}
	jr, err := s.repo.Decide(ctx, d, requestID, data.RequestApproved, role, "")
	if err != nil {
		return data.Member{}, err
	}
	if blocked, err := s.repo.Blocked(ctx, d, jr.UserID); err != nil || blocked {
		if err == nil {
			err = data.ErrForbidden
		}
		return data.Member{}, err
	}
	return s.repo.AddMember(ctx, d, jr.UserID, role)
}

// Reject settles a pending request with a reason; the requester stays out.
func (s *Sessions) Reject(ctx context.Context, actor, id, requestID, reason string) (data.JoinRequest, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return data.JoinRequest{}, invalid("a reason is required to reject a request")
	}
	_, _, d, err := s.as(ctx, actor, id, ActDecideRequest)
	if err != nil {
		return data.JoinRequest{}, err
	}
	return s.repo.Decide(ctx, d, requestID, data.RequestRejected, "", reason)
}

// Members lists the session's members (user id, role). Any member may.
func (s *Sessions) Members(ctx context.Context, actor, id string, q data.Query) (data.Page[data.Member], error) {
	_, _, d, err := s.as(ctx, actor, id, ActView)
	if err != nil {
		return data.Page[data.Member]{}, err
	}
	return s.repo.Members(ctx, d, q)
}

// target loads the member a manager acts on. Nobody can act on the GM.
func (s *Sessions) target(ctx context.Context, d data.Decision, user string) (data.Member, error) {
	m, err := s.repo.Member(ctx, d, user)
	if err != nil {
		return m, err
	}
	if m.Role == data.RoleGM {
		return m, data.ErrForbidden
	}
	return m, nil
}

// SetRole changes a member's role. The gm role cannot be given or taken.
func (s *Sessions) SetRole(ctx context.Context, actor, id, user, role string) (data.Member, error) {
	if !slices.Contains(assignable, role) {
		return data.Member{}, invalid("role must be one of %s", strings.Join(assignable, ", "))
	}
	_, _, d, err := s.as(ctx, actor, id, ActManageMembers)
	if err != nil {
		return data.Member{}, err
	}
	if _, err := s.target(ctx, d, user); err != nil {
		return data.Member{}, err
	}
	return s.repo.SetRole(ctx, d, user, role)
}

// Remove ends a member's access.
func (s *Sessions) Remove(ctx context.Context, actor, id, user string) error {
	_, _, d, err := s.as(ctx, actor, id, ActManageMembers)
	if err != nil {
		return err
	}
	if _, err := s.target(ctx, d, user); err != nil {
		return err
	}
	return s.repo.RemoveMember(ctx, d, user)
}

// Leave removes actor from the session. The GM cannot leave; they delete the session.
func (s *Sessions) Leave(ctx context.Context, actor, id string) error {
	_, m, d, err := s.as(ctx, actor, id, ActView)
	if err != nil {
		return err
	}
	if !Allowed(m.Role, ActLeave) {
		return invalid("the GM cannot leave; delete the session instead")
	}
	return s.repo.RemoveMember(ctx, d, actor)
}

// Block keeps user out of the session, removing them and any pending request.
func (s *Sessions) Block(ctx context.Context, actor, id, user string) error {
	_, _, d, err := s.as(ctx, actor, id, ActBlock)
	if err != nil {
		return err
	}
	if user == actor {
		return invalid("you cannot block yourself")
	}
	if _, err := s.target(ctx, d, user); err != nil && !errors.Is(err, data.ErrNotFound) {
		return err
	}
	if err := s.repo.AddBlock(ctx, d, user); err != nil {
		return err
	}
	if err := s.repo.DropRequest(ctx, d, user); err != nil {
		return err
	}
	if err := s.repo.RemoveMember(ctx, d, user); err != nil && !errors.Is(err, data.ErrNotFound) {
		return err
	}
	return nil
}

// Unblock lets user join again.
func (s *Sessions) Unblock(ctx context.Context, actor, id, user string) error {
	_, _, d, err := s.as(ctx, actor, id, ActBlock)
	if err != nil {
		return err
	}
	return s.repo.RemoveBlock(ctx, d, user)
}

// Blocks lists the blocked users.
func (s *Sessions) Blocks(ctx context.Context, actor, id string, q data.Query) (data.Page[data.Block], error) {
	_, _, d, err := s.as(ctx, actor, id, ActBlock)
	if err != nil {
		return data.Page[data.Block]{}, err
	}
	return s.repo.Blocks(ctx, d, q)
}
