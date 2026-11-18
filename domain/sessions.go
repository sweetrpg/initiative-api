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

	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/store"
)

// Action is something a session member may try to do. Allowed is the one place the role rules
// live (spec initiative-sessions, "Roles").
type Action string

// Session actions.
const (
	ActView            Action = "view"
	ActInvite          Action = "invite"
	ActDecideRequest   Action = "decide_request"
	ActManageMembers   Action = "manage_members"
	ActBlock           Action = "block"
	ActLeave           Action = "leave"
	ActDelete          Action = "delete_session"
	ActUpdate          Action = "update_session"
	ActCreateEncounter Action = "create_encounter"
)

var allowed = map[Action][]string{
	ActView:            {data.RoleGM, data.RoleCoGM, data.RolePlayer, data.RoleObserver},
	ActInvite:          {data.RoleGM, data.RoleCoGM},
	ActDecideRequest:   {data.RoleGM, data.RoleCoGM},
	ActManageMembers:   {data.RoleGM, data.RoleCoGM},
	ActBlock:           {data.RoleGM, data.RoleCoGM},
	ActLeave:           {data.RoleCoGM, data.RolePlayer, data.RoleObserver},
	ActDelete:          {data.RoleGM},
	ActUpdate:          {data.RoleGM},
	ActCreateEncounter: {data.RoleGM, data.RoleCoGM},
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
var sessionKeys = []string{KeyMembersPerSession, KeySavedEncounters}

// sessionFlags are the owner flags recorded on a session beside the limits.
var sessionFlags = []string{KeyGameSystemsExtra}

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
	// AllowCoGMEncounters is the "allow a co-GM to create an encounter" setting. It is set only for
	// the GM and co-GMs; players and observers never see it.
	AllowCoGMEncounters *bool `json:"allow_co_gm_create_encounter,omitempty"`
}

// viewOf builds the view of sess for a member holding role.
func viewOf(sess data.Session, role string) SessionView {
	v := SessionView{Session: sess, Role: role}
	if role == data.RoleGM || role == data.RoleCoGM {
		v.AllowCoGMEncounters = &sess.AllowCoGMCreateEncounter
	}
	return v
}

// Sessions implements session membership and its rules.
type Sessions struct {
	repo    *data.Sessions
	enc     *data.Encounters
	changes *data.ChangeRequests
	// OwnerRecordMaxAge is how old the GM's recorded entitlements may be when a co-GM acts on
	// them (the configured OWNER_ENTITLEMENTS_MAX_AGE). Zero means seven days.
	OwnerRecordMaxAge time.Duration
	// MaxMembers is the configured maximum of the member limit (SESSION_MEMBERS_MAX): an entitled
	// value above it has no effect. NewSessions sets the default; zero (a Sessions built by hand)
	// fails every member addition closed.
	MaxMembers int
	now        func() time.Time
}

// NewSessions builds the service on db.
func NewSessions(db store.Database) *Sessions {
	return &Sessions{repo: data.NewSessions(db), enc: data.NewEncounters(db), changes: data.NewChangeRequests(db), MaxMembers: constants.DefaultSessionMembersMax, now: func() time.Time { return time.Now().UTC() }}
}

// Viewer returns how actor may see the session's encounters; see data.Sessions.AuthorizeView.
func (s *Sessions) Viewer(ctx context.Context, actor, sessionID string) (data.Viewer, error) {
	return s.repo.AuthorizeView(ctx, actor, sessionID)
}

// Create makes actor the GM of a new session.
// Sessions beyond the GM's entitled count are refused with *EntitlementError; inactive sessions
// count, so a GM over a reduced limit cannot create.
func (s *Sessions) Create(ctx context.Context, actor, name string) (SessionView, error) {
	return s.CreateWith(ctx, actor, name, false)
}

// CreateWith is Create with the co-GM encounter-creation setting chosen up front.
func (s *Sessions) CreateWith(ctx context.Context, actor, name string, allowCoGMCreate bool) (SessionView, error) {
	name, err := cleanSessionName(name)
	if err != nil {
		return SessionView{}, err
	}
	chk, ok := ctx.Value(checkerKey{}).(Checker)
	if !ok {
		return SessionView{}, entitlements.ErrUnavailable
	}
	res, err := chk.Check(ctx, KeySessionsPerGM, entitlements.FailClosed)
	if err != nil {
		return SessionView{}, err
	}
	owned, err := s.repo.Count(ctx, actor)
	if err != nil {
		return SessionView{}, err
	}
	if owned >= int64(res.Limit) {
		return SessionView{}, &EntitlementError{Key: KeySessionsPerGM, Limit: res.Limit}
	}
	auth, err := authorityOf(ctx)
	if err != nil {
		return SessionView{}, err
	}
	id, err := auth.svc.Mint(ctx, auth.token)
	if err != nil {
		return SessionView{}, err
	}
	sess := data.Session{Name: name, AllowCoGMCreateEncounter: allowCoGMCreate}
	if err := s.repo.Open(ctx, actor, id, &sess); err != nil {
		// Frees the mint slot; garbage collection covers a failure here.
		if eerr := s.endSession(ctx, id); eerr != nil {
			slog.WarnContext(ctx, "ending a session whose creation failed did not work; garbage collection will close it", "session", id, "error", eerr)
		}
		return SessionView{}, err
	}
	return viewOf(sess, data.RoleGM), nil
}

func cleanSessionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxSessionName {
		return "", invalid("session name is required and at most %d characters", maxSessionName)
	}
	return name, nil
}

// SessionChanges are the settings the GM may change on a session; nil leaves one as it is.
type SessionChanges struct {
	Name                *string
	AllowCoGMEncounters *bool
}

// Update changes the session's settings at rev. Only the GM may: the relation asked of OpenFGA is
// gm and the write is scoped to the session's owner, so a co-GM cannot change the setting that
// grants co-GMs a power.
func (s *Sessions) Update(ctx context.Context, actor, id string, rev int64, ch SessionChanges) (SessionView, error) {
	_, m, d, err := s.as(ctx, actor, id, ActUpdate)
	if err != nil {
		return SessionView{}, err
	}
	set := map[string]any{}
	if ch.Name != nil {
		name, err := cleanSessionName(*ch.Name)
		if err != nil {
			return SessionView{}, err
		}
		set["name"] = name
	}
	if ch.AllowCoGMEncounters != nil {
		set["allow_co_gm_create_encounter"] = *ch.AllowCoGMEncounters
	}
	if len(set) == 0 {
		return SessionView{}, invalid("nothing to update")
	}
	sess, err := s.repo.Update(ctx, d, rev, set)
	if err != nil {
		return SessionView{}, err
	}
	return viewOf(sess, m.Role), nil
}

// actionRelation is the OpenFGA relation on the session each action needs. The role rules in
// Allowed still apply on top: OpenFGA says whether the caller may touch the session at all.
var actionRelation = map[Action]string{
	ActView: "can_view", ActLeave: "can_view",
	ActInvite: "can_edit", ActBlock: "can_edit", ActManageMembers: "can_edit",
	ActDecideRequest:   "can_approve",
	ActDelete:          "gm",
	ActUpdate:          "gm",
	ActCreateEncounter: "can_edit",
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
	limits, flags := make(map[string]int, len(sessionKeys)), make(map[string]bool, len(sessionFlags))
	for _, key := range append(slices.Clone(sessionKeys), sessionFlags...) {
		res, err := chk.Check(ctx, key, entitlements.FailOpen)
		if err != nil || res.Degraded || res.Stale {
			slog.WarnContext(ctx, "session owner entitlements not refreshed", "session", sess.ID, "key", key, "error", err)
			return sess
		}
		if slices.Contains(sessionFlags, key) {
			flags[key] = res.Flag
		} else {
			limits[key] = res.Limit
		}
	}
	fresh := sess.OwnerLimitsAt != nil && s.now().Sub(*sess.OwnerLimitsAt) < ownerLimitsRefreshEvery
	if fresh && maps.Equal(limits, sess.OwnerLimits) && maps.Equal(flags, sess.OwnerFlags) {
		return sess
	}
	if err := s.repo.RecordOwnerLimits(ctx, d, limits, flags); err != nil {
		slog.WarnContext(ctx, "session owner entitlements not recorded", "session", sess.ID, "error", err)
		return sess
	}
	now := s.now()
	sess.OwnerLimits, sess.OwnerFlags, sess.OwnerLimitsAt = limits, flags, &now
	return sess
}

// Items exposes the sessions as limited items, so the limit reconciler can deactivate those beyond
// the owner's entitled count.
func (s *Sessions) Items() Limited { return s.repo.Limits() }

// Get returns the session with actor's role. Only members may read it.
func (s *Sessions) Get(ctx context.Context, actor, id string) (SessionView, error) {
	sess, m, _, err := s.as(ctx, actor, id, ActView)
	return viewOf(sess, m.Role), err
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
		out.Items = append(out.Items, viewOf(sess, m.Role))
	}
	return out, nil
}

// Delete soft-deletes the session, ends it at auth-api (every tuple and the mint), ends every
// membership, and turns the session's encounters into private encounters of the GM
// (initiative-sessions, "Session deletion"): session_id and controllers are cleared and the GM
// becomes the owner, including of encounters a co-GM authored. Only the GM may; the owner-scoped
// delete enforces it a second time. The soft delete is the commit point. Every later step is
// idempotent and is attempted even if an earlier one fails, and the errors are returned together;
// calling Delete again (the GM, same id) then finishes what is left, without the revision, because
// a deleted session can no longer be authorized through OpenFGA. On such a retry auth-api may
// already have dropped the GM's own tuple, so a denial when ending the session counts as ended.
func (s *Sessions) Delete(ctx context.Context, actor, id string, rev int64) error {
	_, _, d, err := s.as(ctx, actor, id, ActDelete)
	resumed := false
	switch {
	case err == nil:
		if err := s.repo.SoftDelete(ctx, d, rev); err != nil {
			return err
		}
	case errors.Is(err, data.ErrForbidden) || errors.Is(err, data.ErrNotFound):
		// Perhaps a deletion this GM started and did not finish. Anyone else, and a session that
		// was never deleted, gets the original answer.
		rd, rerr := s.repo.ResumeDeletion(ctx, actor, id)
		if rerr != nil {
			return err
		}
		d, resumed = rd, true
	default:
		return err
	}
	// End the session at auth-api before dropping the member records: once the session record is
	// soft-deleted a retry cannot authorize the call through OpenFGA, so it runs first.
	endErr := s.endSession(ctx, d.SessionID())
	if resumed && errors.Is(endErr, relations.ErrDenied) {
		endErr = nil
	}
	return errors.Join(
		endErr,
		s.repo.DropSession(ctx, d),
		s.changes.CancelSession(ctx, d),
		s.enc.ReleaseSession(ctx, d),
	)
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
	sess, am, d, err := s.as(ctx, actor, id, ActDecideRequest)
	if err != nil {
		return data.Member{}, err
	}
	if !canManageRole(am.Role, role) {
		return data.Member{}, data.ErrForbidden
	}
	// Before the request settles, and failing closed: a stale controller id must not revive on
	// admission. The user is not a member yet, so this cannot race a legitimate assignment, and a
	// failure leaves the request pending to retry rather than approved with no member.
	// Only a request that is not pending (a retried approval, or no such request) skips the scrub;
	// any other lookup error fails closed rather than admit with stale ids intact.
	switch pending, perr := s.repo.PendingUser(ctx, d, requestID); {
	case perr == nil:
		// A request still pending would add a member: the roster must have room, so a full one
		// leaves the request pending for later. A retried approval (not pending) is not an add.
		if err := s.checkRoomForMember(ctx, d, sess, am.Role); err != nil {
			return data.Member{}, err
		}
		if err := s.enc.ScrubController(ctx, d, pending); err != nil {
			return data.Member{}, err
		}
	case !errors.Is(perr, data.ErrNotFound):
		return data.Member{}, perr
	}
	jr, err := s.repo.Decide(ctx, d, requestID, data.RequestApproved, role, "")
	if errors.Is(err, data.ErrConflict) && jr.Status == data.RequestApproved && jr.Role == role {
		return s.finishApproval(ctx, d, am.Role, jr)
	}
	if err != nil {
		return data.Member{}, err
	}
	if blocked, err := s.repo.Blocked(ctx, d, jr.UserID); err != nil || blocked {
		if err == nil {
			err = data.ErrForbidden
		}
		return data.Member{}, err
	}
	m, err := s.repo.AddMember(ctx, d, jr.UserID, role)
	if err != nil {
		return data.Member{}, err
	}
	return m, s.syncGrant(ctx, am.Role, d.SessionID(), m.UserID, role)
}

// errMaxMembersUnset means Sessions.MaxMembers was never configured; additions fail closed.
var errMaxMembersUnset = errors.New("domain: the session member maximum is not configured")

// memberLimit is the session's member limit: the owner's recorded members_per_session value,
// clamped to the configured maximum. It fails closed with ErrOwnerLimitsUnknown when nothing is
// recorded, or (for a co-GM, who cannot read the GM's entitlements) when the record is older than
// the configured maximum age. The GM's own request refreshed the record in as(), but a record the
// GM's own path reads may be arbitrarily old when the entitlements service is down.
func (s *Sessions) memberLimit(sess data.Session, actorRole string) (int, error) {
	if actorRole != data.RoleGM && (sess.OwnerLimitsAt == nil || s.now().Sub(*sess.OwnerLimitsAt) > s.ownerRecordMaxAge()) {
		return 0, ErrOwnerLimitsUnknown
	}
	limit, err := OwnerLimit(sess, KeyMembersPerSession)
	if err != nil {
		return 0, err
	}
	if s.MaxMembers < 1 {
		return 0, errMaxMembersUnset
	}
	return max(0, min(limit, s.MaxMembers)), nil
}

// checkRoomForMember rejects adding a member to a roster that already holds the limit of non-GM
// members, with the typed entitlement error carrying the key and the (clamped) limit. Nothing is
// removed when the limit later falls; this only stops additions. The count and the add are two
// steps, so concurrent approvals can overrun a full roster. Co-GMs are uncapped and one actor can
// approve several pending requests in parallel, so the overrun is bounded by the number of
// requests pending at that moment.
// ponytail: no post-add recount; add one that soft-deletes the member and resets the request to
// pending if overruns matter.
func (s *Sessions) checkRoomForMember(ctx context.Context, d data.Decision, sess data.Session, actorRole string) error {
	limit, err := s.memberLimit(sess, actorRole)
	if err != nil {
		return err
	}
	n, err := s.repo.CountMembers(ctx, d)
	if err != nil {
		return err
	}
	if n >= int64(limit) {
		return &EntitlementError{Key: KeyMembersPerSession, Limit: limit}
	}
	return nil
}

// finishApproval is the retry of an approval that settled but whose tuple write failed: the member
// record exists, so only the tuple is written again. Any other state is the original conflict.
func (s *Sessions) finishApproval(ctx context.Context, d data.Decision, actorRole string, jr data.JoinRequest) (data.Member, error) {
	m, err := s.repo.Member(ctx, d, jr.UserID)
	if err != nil || m.Role != jr.Role {
		return data.Member{}, data.ErrConflict
	}
	return m, s.syncGrant(ctx, actorRole, d.SessionID(), m.UserID, m.Role)
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
	_, am, d, err := s.as(ctx, actor, id, ActManageMembers)
	if err != nil {
		return data.Member{}, err
	}
	t, err := s.target(ctx, d, user)
	if err != nil {
		return data.Member{}, err
	}
	if !canManageRole(am.Role, role) || !canManageRole(am.Role, t.Role) {
		return data.Member{}, data.ErrForbidden
	}
	if t.Role == data.RoleObserver {
		// An observer being promoted starts with no control; failing closed, so a stale id cannot revive.
		if err := s.enc.ScrubController(ctx, d, user); err != nil {
			return data.Member{}, err
		}
	}
	m, err := s.repo.SetRole(ctx, d, user, role)
	if err != nil {
		return data.Member{}, err
	}
	if role == data.RoleObserver {
		s.scrubControllers(ctx, d, user)
	}
	return m, s.syncGrant(ctx, am.Role, d.SessionID(), user, role)
}

// Remove ends a member's access.
func (s *Sessions) Remove(ctx context.Context, actor, id, user string) error {
	_, am, d, err := s.as(ctx, actor, id, ActManageMembers)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, d, user)
	if err != nil {
		return err
	}
	if !canManageRole(am.Role, t.Role) {
		return data.ErrForbidden
	}
	if err := s.repo.RemoveMember(ctx, d, user); err != nil {
		return err
	}
	s.scrubControllers(ctx, d, user)
	s.syncRevoke(ctx, am.Role, d.SessionID(), user, t.Role)
	return nil
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
	if err := s.repo.RemoveMember(ctx, d, actor); err != nil {
		return err
	}
	s.scrubControllers(ctx, d, actor)
	s.syncRevoke(ctx, m.Role, d.SessionID(), actor, m.Role)
	return nil
}

// Block keeps user out of the session, removing them and any pending request.
func (s *Sessions) Block(ctx context.Context, actor, id, user string) error {
	_, am, d, err := s.as(ctx, actor, id, ActBlock)
	if err != nil {
		return err
	}
	if user == actor {
		return invalid("you cannot block yourself")
	}
	t, err := s.target(ctx, d, user)
	if err != nil && !errors.Is(err, data.ErrNotFound) {
		return err
	}
	if err == nil && !canManageRole(am.Role, t.Role) {
		return data.ErrForbidden
	}
	if err := s.repo.AddBlock(ctx, d, user); err != nil {
		return err
	}
	if err := s.repo.DropRequest(ctx, d, user); err != nil {
		return err
	}
	switch err := s.repo.RemoveMember(ctx, d, user); {
	case err == nil:
		s.scrubControllers(ctx, d, user)
		s.syncRevoke(ctx, am.Role, d.SessionID(), user, t.Role)
	case !errors.Is(err, data.ErrNotFound):
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

// scrubControllers removes user from every combatant's controllers in the session's encounters,
// so a removed, blocked, or demoted member cannot regain control by rejoining or being promoted.
// It runs after the membership write and again before admission; it is idempotent and best
// effort, because the live Viewer already denies a non-player any control (domain.controls).
func (s *Sessions) scrubControllers(ctx context.Context, d data.Decision, user string) {
	if err := s.enc.ScrubController(ctx, d, user); err != nil {
		slog.WarnContext(ctx, "scrubbing a member's combatant controls failed; admission scrubs again", "session", d.SessionID(), "user", user, "error", err)
	}
	// Their pending change requests can no longer be approved (approval needs a current player
	// controller), so close them instead of leaving them in the GM's queue.
	if err := s.changes.CancelPending(ctx, d, user); err != nil {
		slog.WarnContext(ctx, "cancelling a member's pending change requests failed", "session", d.SessionID(), "user", user, "error", err)
	}
}
