package domain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// sctx carries a checker that grants 10 of every limit, so session tests run unlimited by default.
var sctx = WithChecker(context.Background(), stubChecker{limit: 10})

func newSessions(t *testing.T) *Sessions {
	t.Helper()
	return NewSessions(fake.NewDB())
}

// table seats a GM (gm), co-GM (cogm), player (p1), and observer (obs) in one session.
func table(t *testing.T) (*Sessions, string) {
	t.Helper()
	s := newSessions(t)
	sess, err := s.Create(sctx, "gm", "Friday")
	if err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"cogm": data.RoleCoGM, "p1": data.RolePlayer, "obs": data.RoleObserver} {
		join(t, s, sess.ID, user, role)
	}
	return s, sess.ID
}

func join(t *testing.T, s *Sessions, sessID, user, role string) {
	t.Helper()
	_, code, err := s.CreateInvite(sctx, "gm", sessID, 0)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := s.Join(sctx, user, code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(sctx, "gm", sessID, jr.ID, role); err != nil {
		t.Fatal(err)
	}
}

func TestRoleMatrix(t *testing.T) {
	tests := []struct {
		action Action
		allow  []string
	}{
		{ActView, []string{"gm", "co_gm", "player", "observer"}},
		{ActInvite, []string{"gm", "co_gm"}},
		{ActDecideRequest, []string{"gm", "co_gm"}},
		{ActManageMembers, []string{"gm", "co_gm"}},
		{ActBlock, []string{"gm", "co_gm"}},
		{ActLeave, []string{"co_gm", "player", "observer"}},
		{ActDelete, []string{"gm"}},
	}
	for _, tt := range tests {
		for _, role := range []string{"gm", "co_gm", "player", "observer", "", "admin"} {
			want := false
			for _, a := range tt.allow {
				want = want || a == role
			}
			if got := Allowed(role, tt.action); got != want {
				t.Errorf("Allowed(%q, %s) = %v, want %v", role, tt.action, got, want)
			}
		}
	}
}

func TestCreatorIsTheOnlyGM(t *testing.T) {
	s, id := table(t)

	v, err := s.Get(sctx, "gm", id)
	page, _ := s.Members(sctx, "gm", id, data.Query{})

	gms := 0
	for _, m := range page.Items {
		if m.Role == data.RoleGM {
			gms++
		}
	}
	if err != nil || v.Role != data.RoleGM || v.OwnerUserID != "gm" || gms != 1 || len(page.Items) != 4 {
		t.Fatalf("view = %+v (%v), gms = %d, members = %d", v, err, gms, len(page.Items))
	}
}

func TestCreateValidatesTheName(t *testing.T) {
	s := newSessions(t)
	for _, name := range []string{"", "  ", string(make([]rune, 101))} {
		if _, err := s.Create(sctx, "gm", name); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("Create(%.5q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestOnlyMembersSeeASession(t *testing.T) {
	s, id := table(t)
	if _, err := s.Get(sctx, "stranger", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("non-member = %v, want ErrForbidden", err)
	}
	if _, err := s.Get(sctx, "obs", id); err != nil {
		t.Errorf("observer = %v", err)
	}
	if _, err := s.Get(sctx, "gm", "64b7f0f0f0f0f0f0f0f0f0f0"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("unknown = %v, want ErrNotFound", err)
	}
	page, _ := s.List(sctx, "stranger", data.Query{})
	if len(page.Items) != 0 {
		t.Errorf("stranger lists %d sessions", len(page.Items))
	}
	if page, _ := s.List(sctx, "p1", data.Query{}); len(page.Items) != 1 || page.Items[0].Role != data.RolePlayer {
		t.Errorf("player's sessions = %+v", page.Items)
	}
}

func TestOnlyGMAndCoGMManage(t *testing.T) {
	s, id := table(t)
	for _, user := range []string{"p1", "obs", "stranger"} {
		if _, _, err := s.CreateInvite(sctx, user, id, 0); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s invite = %v, want ErrForbidden", user, err)
		}
		if _, err := s.Requests(sctx, user, id, true, data.Query{}); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s requests = %v, want ErrForbidden", user, err)
		}
		if err := s.Remove(sctx, user, id, "p1"); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s remove = %v, want ErrForbidden", user, err)
		}
		if err := s.Block(sctx, user, id, "x"); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s block = %v, want ErrForbidden", user, err)
		}
	}
	if _, _, err := s.CreateInvite(sctx, "cogm", id, 0); err != nil {
		t.Errorf("co-GM invite = %v", err)
	}
}

func TestCoGMCannotDeleteTheSessionOrTouchTheGM(t *testing.T) {
	s, id := table(t)
	v, _ := s.Get(sctx, "gm", id)

	if err := s.Delete(sctx, "cogm", id, v.Revision); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("co-GM delete = %v, want ErrForbidden", err)
	}
	if err := s.Remove(sctx, "cogm", id, "gm"); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("remove GM = %v, want ErrForbidden", err)
	}
	if _, err := s.SetRole(sctx, "cogm", id, "gm", data.RolePlayer); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("demote GM = %v, want ErrForbidden", err)
	}
	if err := s.Block(sctx, "cogm", id, "gm"); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("block GM = %v, want ErrForbidden", err)
	}
	if _, err := s.SetRole(sctx, "cogm", id, "p1", data.RoleGM); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("promote to GM = %v, want ErrInvalid (exactly one GM)", err)
	}
	if _, err := s.Get(sctx, "gm", id); err != nil {
		t.Errorf("session gone after refused deletes: %v", err)
	}
}

func TestJoinNeedsApprovalAndGrantsNothingBefore(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)

	jr, err := s.Join(sctx, "newbie", code)

	if err != nil || jr.Status != data.RequestPending {
		t.Fatalf("join = %+v, %v", jr, err)
	}
	if _, err := s.Get(sctx, "newbie", sess.ID); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("pending user reads session = %v, want ErrForbidden", err)
	}
	if again, err := s.Join(sctx, "newbie", code); err != nil || again.ID != jr.ID {
		t.Fatalf("second join = %+v, %v; want the same pending request", again, err)
	}
	m, err := s.Approve(sctx, "gm", sess.ID, jr.ID, data.RolePlayer)
	if err != nil || m.Role != data.RolePlayer {
		t.Fatalf("approve = %+v, %v", m, err)
	}
	if _, err := s.Get(sctx, "newbie", sess.ID); err != nil {
		t.Fatalf("approved member = %v", err)
	}
	if _, err := s.Join(sctx, "newbie", code); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("member joins again = %v, want ErrConflict", err)
	}
	if _, err := s.Approve(sctx, "gm", sess.ID, jr.ID, data.RolePlayer); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("approve twice = %v, want ErrConflict", err)
	}
}

func TestRejectNeedsAReasonAndAllowsAnotherTry(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	jr, _ := s.Join(sctx, "u", code)

	if _, err := s.Reject(sctx, "gm", sess.ID, jr.ID, " "); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("reject without reason = %v, want ErrInvalid", err)
	}
	rej, err := s.Reject(sctx, "gm", sess.ID, jr.ID, "not in my group")

	if err != nil || rej.Status != data.RequestRejected || rej.Reason != "not in my group" || rej.DecidedBy != "gm" {
		t.Fatalf("reject = %+v, %v", rej, err)
	}
	if _, err := s.Get(sctx, "u", sess.ID); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("rejected user reads session = %v", err)
	}
	if again, err := s.Join(sctx, "u", code); err != nil || again.Status != data.RequestPending || again.Reason != "" {
		t.Fatalf("rejoin after rejection = %+v, %v; want a fresh pending request", again, err)
	}
}

func TestApproveRejectsRolesThatAreNotAssignable(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	jr, _ := s.Join(sctx, "u", code)
	for _, role := range []string{"gm", "", "admin"} {
		if _, err := s.Approve(sctx, "gm", sess.ID, jr.ID, role); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("approve as %q = %v, want ErrInvalid", role, err)
		}
	}
	if page, _ := s.Requests(sctx, "gm", sess.ID, true, data.Query{}); len(page.Items) != 1 {
		t.Errorf("pending after refused approvals = %d, want 1", len(page.Items))
	}
}

func TestInviteStatesAllFailTheSameWayAndCreateNoRequest(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	revoked, codeRevoked, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	if _, err := s.RevokeInvite(sctx, "gm", sess.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, codeBlocked, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	if err := s.Block(sctx, "gm", sess.ID, "bad"); err != nil {
		t.Fatal(err)
	}
	_, codeExpired, _ := s.CreateInvite(sctx, "gm", sess.ID, time.Hour)
	live := s.now
	s.now = func() time.Time { return live().Add(2 * time.Hour) }

	tests := []struct{ name, user, code string }{
		{"expired", "u", codeExpired},
		{"revoked", "u", codeRevoked},
		{"blocked user", "bad", codeBlocked},
		{"unknown code", "u", "NOSUCHCODE"},
		{"empty", "u", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Join(sctx, tt.user, tt.code); !errors.Is(err, ErrInviteInvalid) {
				t.Fatalf("err = %v, want ErrInviteInvalid", err)
			}
		})
	}
	s.now = live
	if page, _ := s.Requests(sctx, "gm", sess.ID, false, data.Query{}); len(page.Items) != 0 {
		t.Fatalf("%d requests were created by failed joins", len(page.Items))
	}
}

func TestRevokedAndBlockedFailEvenBeforeExpiry(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	in, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	if _, err := s.Join(sctx, "ok", code); err != nil {
		t.Fatalf("a live invite failed: %v", err)
	}
	if _, err := s.RevokeInvite(sctx, "gm", sess.ID, in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(sctx, "other", code); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked, unexpired invite = %v, want ErrInviteInvalid", err)
	}
}

func TestInviteLifetimeIsBounded(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	for _, ttl := range []time.Duration{time.Minute, 31 * 24 * time.Hour, -time.Hour} {
		if _, _, err := s.CreateInvite(sctx, "gm", sess.ID, ttl); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("ttl %s = %v, want ErrInvalid", ttl, err)
		}
	}
	in, code, err := s.CreateInvite(sctx, "gm", sess.ID, 24*time.Hour)
	if err != nil || len(code) < 16 || in.CodeHash == code || !in.ExpiresAt.After(s.now().Add(23*time.Hour)) {
		t.Fatalf("invite = %+v, %q, %v", in, code, err)
	}
}

func TestRemovedMemberLosesAccessAtOnce(t *testing.T) {
	s, id := table(t)

	if err := s.Remove(sctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(sctx, "p1", id); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("removed member = %v, want ErrForbidden", err)
	}
	if err := s.Remove(sctx, "gm", id, "p1"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("remove twice = %v, want ErrNotFound", err)
	}
}

func TestRemovedMemberDoesNotSeeTheSessionInList(t *testing.T) {
	s, id := table(t)
	if err := s.Remove(sctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	page, err := s.List(sctx, "p1", data.Query{})

	if err != nil || len(page.Items) != 0 {
		t.Fatalf("list for removed member = %+v, %v; want empty", page.Items, err)
	}
}

func TestBlockRemovesMemberAndStopsRejoining(t *testing.T) {
	s, id := table(t)
	_, code, _ := s.CreateInvite(sctx, "gm", id, 0)

	if err := s.Block(sctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(sctx, "p1", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("blocked member = %v, want ErrForbidden", err)
	}
	if _, err := s.Join(sctx, "p1", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("blocked user joins = %v, want ErrInviteInvalid", err)
	}
	if err := s.Block(sctx, "gm", id, "gm"); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("block self = %v, want ErrInvalid", err)
	}
	if err := s.Unblock(sctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(sctx, "p1", code); err != nil {
		t.Errorf("join after unblock = %v", err)
	}
	if err := s.Unblock(sctx, "gm", id, "p1"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("unblock twice = %v, want ErrNotFound", err)
	}
}

func TestBlockDropsAPendingRequest(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	if _, err := s.Join(sctx, "u", code); err != nil {
		t.Fatal(err)
	}

	if err := s.Block(sctx, "gm", sess.ID, "u"); err != nil {
		t.Fatal(err)
	}

	if page, _ := s.Requests(sctx, "gm", sess.ID, false, data.Query{}); len(page.Items) != 0 {
		t.Fatalf("requests after block = %d, want 0", len(page.Items))
	}
}

func TestLeaveRules(t *testing.T) {
	s, id := table(t)
	if err := s.Leave(sctx, "gm", id); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("GM leaves = %v, want ErrInvalid", err)
	}
	if err := s.Leave(sctx, "stranger", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("stranger leaves = %v, want ErrForbidden", err)
	}
	if err := s.Leave(sctx, "obs", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(sctx, "obs", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("after leaving = %v, want ErrForbidden", err)
	}
}

func TestSetRoleMovesAMemberBetweenRoles(t *testing.T) {
	s, id := table(t)

	m, err := s.SetRole(sctx, "gm", id, "p1", data.RoleCoGM)

	if err != nil || m.Role != data.RoleCoGM {
		t.Fatalf("set role = %+v, %v", m, err)
	}
	if _, _, err := s.CreateInvite(sctx, "p1", id, 0); err != nil {
		t.Fatalf("promoted co-GM cannot invite: %v", err)
	}
	if _, err := s.SetRole(sctx, "gm", id, "stranger", data.RolePlayer); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("non-member = %v, want ErrNotFound", err)
	}
	if _, err := s.SetRole(sctx, "gm", id, "p1", "boss"); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("unknown role = %v, want ErrInvalid", err)
	}
}

func TestDeletingTheSessionEndsEveryMembershipAndInvite(t *testing.T) {
	s, id := table(t)
	_, code, _ := s.CreateInvite(sctx, "gm", id, 0)
	v, _ := s.Get(sctx, "gm", id)

	if err := s.Delete(sctx, "gm", id, v.Revision); err != nil {
		t.Fatal(err)
	}

	for _, user := range []string{"gm", "cogm", "p1", "obs"} {
		if _, err := s.Get(sctx, user, id); !errors.Is(err, data.ErrNotFound) {
			t.Errorf("%s after delete = %v, want ErrNotFound", user, err)
		}
	}
	if _, err := s.Join(sctx, "u", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("join after delete = %v, want ErrInviteInvalid", err)
	}
	if page, _ := s.List(sctx, "p1", data.Query{}); len(page.Items) != 0 {
		t.Errorf("player still lists %d sessions", len(page.Items))
	}
}

func TestDeleteChecksRevision(t *testing.T) {
	s, id := table(t)
	if err := s.Delete(sctx, "gm", id, 99); !errors.Is(err, data.ErrRevisionConflict) {
		t.Fatalf("stale delete = %v, want ErrRevisionConflict", err)
	}
}

func TestMalformedIDsAreRejected(t *testing.T) {
	s, id := table(t)
	if _, err := s.Get(sctx, "gm", "junk"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk session id = %v, want ErrNotFound", err)
	}
	if err := s.Block(sctx, "gm", id, `{"$ne":1}`); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("operator-shaped user id = %v, want ErrInvalid", err)
	}
	if _, err := s.RevokeInvite(sctx, "gm", id, "junk"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk invite id = %v, want ErrNotFound", err)
	}
	if _, err := s.Approve(sctx, "gm", id, "junk", data.RolePlayer); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk request id = %v, want ErrNotFound", err)
	}
}

func TestRevokeInviteTwiceIsHarmless(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	in, _, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)

	first, err1 := s.RevokeInvite(sctx, "gm", sess.ID, in.ID)
	again, err2 := s.RevokeInvite(sctx, "gm", sess.ID, in.ID)

	if err1 != nil || err2 != nil || first.RevokedAt == nil || again.RevokedAt == nil {
		t.Fatalf("revoke = %+v %v / %+v %v", first, err1, again, err2)
	}
	if page, _ := s.Invites(sctx, "gm", sess.ID, data.Query{}); len(page.Items) != 1 || page.Items[0].RevokedAt == nil {
		t.Fatalf("invites = %+v", page.Items)
	}
}

func TestJoinHealsAnApprovalThatNeverAddedTheMember(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(sctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(sctx, "gm", sess.ID, 0)
	jr, _ := s.Join(sctx, "u", code)
	if _, err := s.repo.Decide(sctx, "gm", sess.ID, jr.ID, data.RequestApproved, data.RolePlayer, ""); err != nil {
		t.Fatal(err) // settled, but no member was added
	}

	again, err := s.Join(sctx, "u", code)

	if err != nil || again.Status != data.RequestPending {
		t.Fatalf("join = %+v, %v; want a fresh pending request", again, err)
	}
	if _, err := s.Approve(sctx, "gm", sess.ID, again.ID, data.RolePlayer); err != nil {
		t.Fatalf("approve after healing = %v", err)
	}
}

func TestRemovedPlayerCanJoinAgainThroughAnInvite(t *testing.T) {
	s, id := table(t)
	if err := s.Remove(sctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	join(t, s, id, "p1", data.RoleObserver)

	v, err := s.Get(sctx, "p1", id)
	if err != nil || v.Role != data.RoleObserver {
		t.Fatalf("rejoined = %+v, %v", v, err)
	}
}

// meter is a checker whose answer tests can change and whose calls they can count.
type meter struct {
	limit    int
	err      error
	degraded bool
	calls    int
}

func (m *meter) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	m.calls++
	if m.err != nil {
		return entitlements.Result{}, m.err
	}
	return entitlements.Result{Key: key, Type: "limit", Limit: m.limit, Degraded: m.degraded}, nil
}

func TestCreateStopsAtTheSessionLimit(t *testing.T) {
	s := newSessions(t)
	c := WithChecker(context.Background(), stubChecker{limit: 1})
	if _, err := s.Create(c, "gm", "One"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Create(c, "gm", "Two")

	var ee *EntitlementError
	if !errors.As(err, &ee) || ee.Key != KeySessionsPerGM || ee.Limit != 1 {
		t.Fatalf("second create = %v, want EntitlementError for %s limit 1", err, KeySessionsPerGM)
	}
}

func TestCreateFailsClosedWhenEntitlementsAreDown(t *testing.T) {
	s := newSessions(t)
	down := WithChecker(context.Background(), stubChecker{err: entitlements.ErrUnavailable})
	for name, c := range map[string]context.Context{"outage": down, "no checker": context.Background()} {
		if _, err := s.Create(c, "gm", "One"); !errors.Is(err, entitlements.ErrUnavailable) {
			t.Errorf("%s: create = %v, want ErrUnavailable", name, err)
		}
	}
}

func TestInactiveSessionIsReadableAndDeletableOnly(t *testing.T) {
	s, id := table(t)
	if err := s.repo.SetInactive(sctx, "gm", id, true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(sctx, "p1", id); err != nil {
		t.Errorf("get = %v, want readable", err)
	}
	if _, _, err := s.CreateInvite(sctx, "gm", id, 0); !errors.Is(err, data.ErrInactive) {
		t.Errorf("invite = %v, want ErrInactive", err)
	}
	if _, err := s.Approve(sctx, "gm", id, "x", data.RolePlayer); !errors.Is(err, data.ErrInactive) {
		t.Errorf("approve = %v, want ErrInactive", err)
	}
	if err := s.Leave(sctx, "p1", id); err != nil {
		t.Errorf("leave = %v, want allowed", err)
	}
	sess, _ := s.Get(sctx, "gm", id)
	if err := s.Delete(sctx, "gm", id, sess.Revision); err != nil {
		t.Errorf("delete = %v, want allowed", err)
	}
}

func TestInactiveSessionCannotBeJoined(t *testing.T) {
	s, id := table(t)
	_, code, err := s.CreateInvite(sctx, "gm", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SetInactive(sctx, "gm", id, true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Join(sctx, "newbie", code); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("join = %v, want ErrInviteInvalid", err)
	}
}

func recorded(t *testing.T, s *Sessions, id string) data.Session {
	t.Helper()
	sess, err := s.repo.Live(sctx, "gm", id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestGMRequestRecordsTheOwnersLimitAndUpgradeRefreshesIt(t *testing.T) {
	s := newSessions(t)
	m := &meter{limit: 10}
	c := WithChecker(context.Background(), m)
	sess, err := s.Create(c, "gm", "Friday")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(c, "gm", sess.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := OwnerLimit(recorded(t, s, sess.ID), KeyMembersPerSession); err != nil || v != 10 {
		t.Fatalf("recorded = %d, %v; want 10", v, err)
	}

	m.limit = 20
	if _, err := s.Get(c, "gm", sess.ID); err != nil {
		t.Fatal(err)
	}

	if v, _ := OwnerLimit(recorded(t, s, sess.ID), KeyMembersPerSession); v != 20 {
		t.Fatalf("recorded after upgrade = %d, want 20", v)
	}
}

func TestMemberRequestsUseTheRecordAndNeverCallEntitlements(t *testing.T) {
	s, id := table(t)
	if _, err := s.Get(sctx, "gm", id); err != nil {
		t.Fatal(err)
	}
	m := &meter{err: errors.New("must not be called")}
	c := WithChecker(context.Background(), m)

	if _, err := s.Get(c, "p1", id); err != nil {
		t.Fatalf("player get = %v", err)
	}

	if m.calls != 0 {
		t.Fatalf("entitlements called %d times for a player request", m.calls)
	}
	if v, err := OwnerLimit(recorded(t, s, id), KeyMembersPerSession); err != nil || v != 10 {
		t.Fatalf("record = %d, %v; want the GM's 10 kept", v, err)
	}
}

func TestOutageOnRefreshKeepsTheRecordAndDoesNotFailTheGM(t *testing.T) {
	s := newSessions(t)
	m := &meter{limit: 10}
	c := WithChecker(context.Background(), m)
	sess, _ := s.Create(c, "gm", "Friday")
	if _, err := s.Get(c, "gm", sess.ID); err != nil {
		t.Fatal(err)
	}

	m.err = entitlements.ErrUnavailable
	if _, err := s.Get(c, "gm", sess.ID); err != nil {
		t.Fatalf("get during outage = %v, want success", err)
	}
	m.err, m.degraded, m.limit = nil, true, 0
	if _, err := s.Get(c, "gm", sess.ID); err != nil {
		t.Fatalf("get while degraded = %v, want success", err)
	}

	if v, _ := OwnerLimit(recorded(t, s, sess.ID), KeyMembersPerSession); v != 10 {
		t.Fatalf("record = %d, want the earlier 10 kept", v)
	}
}

func TestSessionWithoutARecordReportsRetryableError(t *testing.T) {
	s := newSessions(t)
	sess, err := s.Create(context.Background(), "gm", "x")
	if err == nil {
		t.Fatalf("create without a checker succeeded: %+v", sess)
	}
	sess, err = s.Create(sctx, "gm", "Friday")
	if err != nil {
		t.Fatal(err)
	}
	fresh := recorded(t, s, sess.ID)
	fresh.OwnerLimits = nil

	if _, err := OwnerLimit(fresh, KeyMembersPerSession); !errors.Is(err, ErrOwnerLimitsUnknown) {
		t.Fatalf("no record = %v, want ErrOwnerLimitsUnknown", err)
	}
}
