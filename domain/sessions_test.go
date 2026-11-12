package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func newSessions(t *testing.T) *Sessions {
	t.Helper()
	return NewSessions(fake.NewDB())
}

// table seats a GM (gm), co-GM (cogm), player (p1), and observer (obs) in one session.
func table(t *testing.T) (*Sessions, string) {
	t.Helper()
	s := newSessions(t)
	sess, err := s.Create(ctx, "gm", "Friday")
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
	_, code, err := s.CreateInvite(ctx, "gm", sessID, 0)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := s.Join(ctx, user, code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "gm", sessID, jr.ID, role); err != nil {
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

	v, err := s.Get(ctx, "gm", id)
	page, _ := s.Members(ctx, "gm", id, data.Query{})

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
		if _, err := s.Create(ctx, "gm", name); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("Create(%.5q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestOnlyMembersSeeASession(t *testing.T) {
	s, id := table(t)
	if _, err := s.Get(ctx, "stranger", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("non-member = %v, want ErrForbidden", err)
	}
	if _, err := s.Get(ctx, "obs", id); err != nil {
		t.Errorf("observer = %v", err)
	}
	if _, err := s.Get(ctx, "gm", "64b7f0f0f0f0f0f0f0f0f0f0"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("unknown = %v, want ErrNotFound", err)
	}
	page, _ := s.List(ctx, "stranger", data.Query{})
	if len(page.Items) != 0 {
		t.Errorf("stranger lists %d sessions", len(page.Items))
	}
	if page, _ := s.List(ctx, "p1", data.Query{}); len(page.Items) != 1 || page.Items[0].Role != data.RolePlayer {
		t.Errorf("player's sessions = %+v", page.Items)
	}
}

func TestOnlyGMAndCoGMManage(t *testing.T) {
	s, id := table(t)
	for _, user := range []string{"p1", "obs", "stranger"} {
		if _, _, err := s.CreateInvite(ctx, user, id, 0); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s invite = %v, want ErrForbidden", user, err)
		}
		if _, err := s.Requests(ctx, user, id, true, data.Query{}); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s requests = %v, want ErrForbidden", user, err)
		}
		if err := s.Remove(ctx, user, id, "p1"); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s remove = %v, want ErrForbidden", user, err)
		}
		if err := s.Block(ctx, user, id, "x"); !errors.Is(err, data.ErrForbidden) {
			t.Errorf("%s block = %v, want ErrForbidden", user, err)
		}
	}
	if _, _, err := s.CreateInvite(ctx, "cogm", id, 0); err != nil {
		t.Errorf("co-GM invite = %v", err)
	}
}

func TestCoGMCannotDeleteTheSessionOrTouchTheGM(t *testing.T) {
	s, id := table(t)
	v, _ := s.Get(ctx, "gm", id)

	if err := s.Delete(ctx, "cogm", id, v.Revision); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("co-GM delete = %v, want ErrForbidden", err)
	}
	if err := s.Remove(ctx, "cogm", id, "gm"); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("remove GM = %v, want ErrForbidden", err)
	}
	if _, err := s.SetRole(ctx, "cogm", id, "gm", data.RolePlayer); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("demote GM = %v, want ErrForbidden", err)
	}
	if err := s.Block(ctx, "cogm", id, "gm"); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("block GM = %v, want ErrForbidden", err)
	}
	if _, err := s.SetRole(ctx, "cogm", id, "p1", data.RoleGM); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("promote to GM = %v, want ErrInvalid (exactly one GM)", err)
	}
	if _, err := s.Get(ctx, "gm", id); err != nil {
		t.Errorf("session gone after refused deletes: %v", err)
	}
}

func TestJoinNeedsApprovalAndGrantsNothingBefore(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)

	jr, err := s.Join(ctx, "newbie", code)

	if err != nil || jr.Status != data.RequestPending {
		t.Fatalf("join = %+v, %v", jr, err)
	}
	if _, err := s.Get(ctx, "newbie", sess.ID); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("pending user reads session = %v, want ErrForbidden", err)
	}
	if again, err := s.Join(ctx, "newbie", code); err != nil || again.ID != jr.ID {
		t.Fatalf("second join = %+v, %v; want the same pending request", again, err)
	}
	m, err := s.Approve(ctx, "gm", sess.ID, jr.ID, data.RolePlayer)
	if err != nil || m.Role != data.RolePlayer {
		t.Fatalf("approve = %+v, %v", m, err)
	}
	if _, err := s.Get(ctx, "newbie", sess.ID); err != nil {
		t.Fatalf("approved member = %v", err)
	}
	if _, err := s.Join(ctx, "newbie", code); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("member joins again = %v, want ErrConflict", err)
	}
	if _, err := s.Approve(ctx, "gm", sess.ID, jr.ID, data.RolePlayer); !errors.Is(err, data.ErrConflict) {
		t.Fatalf("approve twice = %v, want ErrConflict", err)
	}
}

func TestRejectNeedsAReasonAndAllowsAnotherTry(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	jr, _ := s.Join(ctx, "u", code)

	if _, err := s.Reject(ctx, "gm", sess.ID, jr.ID, " "); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("reject without reason = %v, want ErrInvalid", err)
	}
	rej, err := s.Reject(ctx, "gm", sess.ID, jr.ID, "not in my group")

	if err != nil || rej.Status != data.RequestRejected || rej.Reason != "not in my group" || rej.DecidedBy != "gm" {
		t.Fatalf("reject = %+v, %v", rej, err)
	}
	if _, err := s.Get(ctx, "u", sess.ID); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("rejected user reads session = %v", err)
	}
	if again, err := s.Join(ctx, "u", code); err != nil || again.Status != data.RequestPending || again.Reason != "" {
		t.Fatalf("rejoin after rejection = %+v, %v; want a fresh pending request", again, err)
	}
}

func TestApproveRejectsRolesThatAreNotAssignable(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	jr, _ := s.Join(ctx, "u", code)
	for _, role := range []string{"gm", "", "admin"} {
		if _, err := s.Approve(ctx, "gm", sess.ID, jr.ID, role); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("approve as %q = %v, want ErrInvalid", role, err)
		}
	}
	if page, _ := s.Requests(ctx, "gm", sess.ID, true, data.Query{}); len(page.Items) != 1 {
		t.Errorf("pending after refused approvals = %d, want 1", len(page.Items))
	}
}

func TestInviteStatesAllFailTheSameWayAndCreateNoRequest(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	revoked, codeRevoked, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	if _, err := s.RevokeInvite(ctx, "gm", sess.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, codeBlocked, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	if err := s.Block(ctx, "gm", sess.ID, "bad"); err != nil {
		t.Fatal(err)
	}
	_, codeExpired, _ := s.CreateInvite(ctx, "gm", sess.ID, time.Hour)
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
			if _, err := s.Join(ctx, tt.user, tt.code); !errors.Is(err, ErrInviteInvalid) {
				t.Fatalf("err = %v, want ErrInviteInvalid", err)
			}
		})
	}
	s.now = live
	if page, _ := s.Requests(ctx, "gm", sess.ID, false, data.Query{}); len(page.Items) != 0 {
		t.Fatalf("%d requests were created by failed joins", len(page.Items))
	}
}

func TestRevokedAndBlockedFailEvenBeforeExpiry(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	in, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	if _, err := s.Join(ctx, "ok", code); err != nil {
		t.Fatalf("a live invite failed: %v", err)
	}
	if _, err := s.RevokeInvite(ctx, "gm", sess.ID, in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(ctx, "other", code); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked, unexpired invite = %v, want ErrInviteInvalid", err)
	}
}

func TestInviteLifetimeIsBounded(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	for _, ttl := range []time.Duration{time.Minute, 31 * 24 * time.Hour, -time.Hour} {
		if _, _, err := s.CreateInvite(ctx, "gm", sess.ID, ttl); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("ttl %s = %v, want ErrInvalid", ttl, err)
		}
	}
	in, code, err := s.CreateInvite(ctx, "gm", sess.ID, 24*time.Hour)
	if err != nil || len(code) < 16 || in.CodeHash == code || !in.ExpiresAt.After(s.now().Add(23*time.Hour)) {
		t.Fatalf("invite = %+v, %q, %v", in, code, err)
	}
}

func TestRemovedMemberLosesAccessAtOnce(t *testing.T) {
	s, id := table(t)

	if err := s.Remove(ctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, "p1", id); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("removed member = %v, want ErrForbidden", err)
	}
	if err := s.Remove(ctx, "gm", id, "p1"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("remove twice = %v, want ErrNotFound", err)
	}
}

func TestRemovedMemberDoesNotSeeTheSessionInList(t *testing.T) {
	s, id := table(t)
	if err := s.Remove(ctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	page, err := s.List(ctx, "p1", data.Query{})

	if err != nil || len(page.Items) != 0 {
		t.Fatalf("list for removed member = %+v, %v; want empty", page.Items, err)
	}
}

func TestBlockRemovesMemberAndStopsRejoining(t *testing.T) {
	s, id := table(t)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)

	if err := s.Block(ctx, "cogm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, "p1", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("blocked member = %v, want ErrForbidden", err)
	}
	if _, err := s.Join(ctx, "p1", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("blocked user joins = %v, want ErrInviteInvalid", err)
	}
	if err := s.Block(ctx, "gm", id, "gm"); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("block self = %v, want ErrInvalid", err)
	}
	if err := s.Unblock(ctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(ctx, "p1", code); err != nil {
		t.Errorf("join after unblock = %v", err)
	}
	if err := s.Unblock(ctx, "gm", id, "p1"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("unblock twice = %v, want ErrNotFound", err)
	}
}

func TestBlockDropsAPendingRequest(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	if _, err := s.Join(ctx, "u", code); err != nil {
		t.Fatal(err)
	}

	if err := s.Block(ctx, "gm", sess.ID, "u"); err != nil {
		t.Fatal(err)
	}

	if page, _ := s.Requests(ctx, "gm", sess.ID, false, data.Query{}); len(page.Items) != 0 {
		t.Fatalf("requests after block = %d, want 0", len(page.Items))
	}
}

func TestLeaveRules(t *testing.T) {
	s, id := table(t)
	if err := s.Leave(ctx, "gm", id); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("GM leaves = %v, want ErrInvalid", err)
	}
	if err := s.Leave(ctx, "stranger", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("stranger leaves = %v, want ErrForbidden", err)
	}
	if err := s.Leave(ctx, "obs", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "obs", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("after leaving = %v, want ErrForbidden", err)
	}
}

func TestSetRoleMovesAMemberBetweenRoles(t *testing.T) {
	s, id := table(t)

	m, err := s.SetRole(ctx, "gm", id, "p1", data.RoleCoGM)

	if err != nil || m.Role != data.RoleCoGM {
		t.Fatalf("set role = %+v, %v", m, err)
	}
	if _, _, err := s.CreateInvite(ctx, "p1", id, 0); err != nil {
		t.Fatalf("promoted co-GM cannot invite: %v", err)
	}
	if _, err := s.SetRole(ctx, "gm", id, "stranger", data.RolePlayer); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("non-member = %v, want ErrNotFound", err)
	}
	if _, err := s.SetRole(ctx, "gm", id, "p1", "boss"); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("unknown role = %v, want ErrInvalid", err)
	}
}

func TestDeletingTheSessionEndsEveryMembershipAndInvite(t *testing.T) {
	s, id := table(t)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
	v, _ := s.Get(ctx, "gm", id)

	if err := s.Delete(ctx, "gm", id, v.Revision); err != nil {
		t.Fatal(err)
	}

	for _, user := range []string{"gm", "cogm", "p1", "obs"} {
		if _, err := s.Get(ctx, user, id); !errors.Is(err, data.ErrNotFound) {
			t.Errorf("%s after delete = %v, want ErrNotFound", user, err)
		}
	}
	if _, err := s.Join(ctx, "u", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("join after delete = %v, want ErrInviteInvalid", err)
	}
	if page, _ := s.List(ctx, "p1", data.Query{}); len(page.Items) != 0 {
		t.Errorf("player still lists %d sessions", len(page.Items))
	}
}

func TestDeleteChecksRevision(t *testing.T) {
	s, id := table(t)
	if err := s.Delete(ctx, "gm", id, 99); !errors.Is(err, data.ErrRevisionConflict) {
		t.Fatalf("stale delete = %v, want ErrRevisionConflict", err)
	}
}

func TestMalformedIDsAreRejected(t *testing.T) {
	s, id := table(t)
	if _, err := s.Get(ctx, "gm", "junk"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk session id = %v, want ErrNotFound", err)
	}
	if err := s.Block(ctx, "gm", id, `{"$ne":1}`); !errors.Is(err, data.ErrInvalid) {
		t.Errorf("operator-shaped user id = %v, want ErrInvalid", err)
	}
	if _, err := s.RevokeInvite(ctx, "gm", id, "junk"); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk invite id = %v, want ErrNotFound", err)
	}
	if _, err := s.Approve(ctx, "gm", id, "junk", data.RolePlayer); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("junk request id = %v, want ErrNotFound", err)
	}
}

func TestRevokeInviteTwiceIsHarmless(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	in, _, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)

	first, err1 := s.RevokeInvite(ctx, "gm", sess.ID, in.ID)
	again, err2 := s.RevokeInvite(ctx, "gm", sess.ID, in.ID)

	if err1 != nil || err2 != nil || first.RevokedAt == nil || again.RevokedAt == nil {
		t.Fatalf("revoke = %+v %v / %+v %v", first, err1, again, err2)
	}
	if page, _ := s.Invites(ctx, "gm", sess.ID, data.Query{}); len(page.Items) != 1 || page.Items[0].RevokedAt == nil {
		t.Fatalf("invites = %+v", page.Items)
	}
}

func TestJoinHealsAnApprovalThatNeverAddedTheMember(t *testing.T) {
	s := newSessions(t)
	sess, _ := s.Create(ctx, "gm", "Friday")
	_, code, _ := s.CreateInvite(ctx, "gm", sess.ID, 0)
	jr, _ := s.Join(ctx, "u", code)
	if _, err := s.repo.Decide(ctx, "gm", sess.ID, jr.ID, data.RequestApproved, data.RolePlayer, ""); err != nil {
		t.Fatal(err) // settled, but no member was added
	}

	again, err := s.Join(ctx, "u", code)

	if err != nil || again.Status != data.RequestPending {
		t.Fatalf("join = %+v, %v; want a fresh pending request", again, err)
	}
	if _, err := s.Approve(ctx, "gm", sess.ID, again.ID, data.RolePlayer); err != nil {
		t.Fatalf("approve after healing = %v", err)
	}
}

func TestRemovedPlayerCanJoinAgainThroughAnInvite(t *testing.T) {
	s, id := table(t)
	if err := s.Remove(ctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}

	join(t, s, id, "p1", data.RoleObserver)

	v, err := s.Get(ctx, "p1", id)
	if err != nil || v.Role != data.RoleObserver {
		t.Fatalf("rejoined = %+v, %v", v, err)
	}
}
