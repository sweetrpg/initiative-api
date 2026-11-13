package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func newSessionRepo(t *testing.T) (*Sessions, string) {
	t.Helper()
	db := fake.NewDB()
	if err := EnsureIndexes(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewSessions(db)
	r.now = func() time.Time { return clock }
	s := &Session{Name: "Friday"}
	if err := r.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}
	return r, s.ID
}

func TestAddMemberRejectsDuplicatesAndBadIDs(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}

	if _, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RoleObserver); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v, want ErrConflict", err)
	}
	if _, err := r.AddMember(ctx, dc(t, "gm", id), `{"$gt":""}`, RolePlayer); !errors.Is(err, ErrInvalid) {
		t.Errorf("operator-shaped user = %v, want ErrInvalid", err)
	}
	if m, err := r.Member(ctx, dc(t, "gm", id), "u1"); err != nil || m.Role != RolePlayer {
		t.Errorf("member = %+v, %v", m, err)
	}
}

func TestSetRoleAndRemoveMemberReportMissingMembers(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.SetRole(ctx, dc(t, "gm", id), "ghost", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role = %v, want ErrNotFound", err)
	}
	if err := r.RemoveMember(ctx, dc(t, "gm", id), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove = %v, want ErrNotFound", err)
	}
}

func TestBlocksAreIdempotentAndLiftable(t *testing.T) {
	r, id := newSessionRepo(t)

	for range 2 {
		if err := r.AddBlock(ctx, dc(t, "gm", id), "bad"); err != nil {
			t.Fatal(err)
		}
	}

	if blocked, err := r.Blocked(ctx, dc(t, "gm", id), "bad"); err != nil || !blocked {
		t.Fatalf("blocked = %v, %v", blocked, err)
	}
	if err := r.RemoveBlock(ctx, dc(t, "gm", id), "bad"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveBlock(ctx, dc(t, "gm", id), "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lift twice = %v, want ErrNotFound", err)
	}
}

func TestRequestJoinStates(t *testing.T) {
	r, id := newSessionRepo(t)
	first, err := r.RequestJoin(ctx, ticketFor(t, r, id, "u1"))
	if err != nil || first.Status != RequestPending {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if same, _ := r.RequestJoin(ctx, ticketFor(t, r, id, "u1")); same.ID != first.ID {
		t.Fatalf("second request got a new id")
	}
	if _, err := r.Decide(ctx, dc(t, "gm", id), first.ID, RequestRejected, "", "no"); err != nil {
		t.Fatal(err)
	}

	again, err := r.RequestJoin(ctx, ticketFor(t, r, id, "u1"))

	if err != nil || again.Status != RequestPending || again.Reason != "" || again.DecidedBy != "" {
		t.Fatalf("after rejection = %+v, %v; want a clean pending request", again, err)
	}
	if _, err := r.Decide(ctx, dc(t, "gm", id), "junk", RequestApproved, RolePlayer, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("junk request id = %v, want ErrNotFound", err)
	}
	if _, err := r.Decide(ctx, dc(t, "gm", id), testID("z"), RequestApproved, RolePlayer, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown request = %v, want ErrNotFound", err)
	}
}

func TestDropSessionRemovesEveryMembershipRecord(t *testing.T) {
	db := fake.NewDB()
	r := NewSessions(db)
	s := &Session{Name: "Friday"}
	if err := r.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, dc(t, "gm", s.ID), "gm", RoleGM); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, dc(t, "gm", s.ID), "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddInvite(ctx, dc(t, "gm", s.ID), "hash", clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RequestJoin(ctx, ticketFor(t, r, s.ID, "u2")); err != nil {
		t.Fatal(err)
	}
	if err := r.AddBlock(ctx, dc(t, "gm", s.ID), "u3"); err != nil {
		t.Fatal(err)
	}

	if err := r.DropSession(ctx, dc(t, "gm", s.ID)); err != nil {
		t.Fatal(err)
	}

	for _, c := range []string{CollectionSessionMembers, CollectionSessionInvites, CollectionSessionRequests, CollectionSessionBlocks} {
		docs := db.Fake(c).Docs()
		if len(docs) == 0 {
			t.Errorf("%s was hard-deleted; PADR-0027 requires soft delete", c)
		}
		for _, d := range docs {
			if d["deleted_at"] == nil || d["deleted_by"] != "gm" || d["live"] != nil {
				t.Errorf("%s record not soft-deleted by gm: %v", c, d)
			}
		}
	}
	if _, err := r.Member(ctx, dc(t, "gm", s.ID), "u1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped member still readable: %v", err)
	}
	if _, err := r.ResolveInvite(ctx, "gm", "hash", r.now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped invite still resolves: %v", err)
	}
}

func TestRemovedMemberCanBeAddedAgain(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, dc(t, "gm", id), "u1"); err != nil {
		t.Fatal(err)
	}

	m, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RoleObserver)

	if err != nil || m.Role != RoleObserver {
		t.Fatalf("re-add = %+v, %v; the soft-deleted row must not block it", m, err)
	}
	if page, _ := r.Members(ctx, dc(t, "gm", id), Query{}); len(page.Items) != 1 {
		t.Fatalf("members = %d, want only the live one", len(page.Items))
	}
}

func TestRemovedMemberIsNotListedUnderTheirSessions(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, dc(t, "gm", id), "u1"); err != nil {
		t.Fatal(err)
	}

	page, err := r.SessionIDsOf(ctx, "u1", Query{})

	if err != nil || len(page.Items) != 0 {
		t.Fatalf("sessions of removed member = %+v, %v; want none", page.Items, err)
	}
}

func TestEveryDataCallRefusesTheZeroDecision(t *testing.T) {
	r, id := newSessionRepo(t)
	var d Decision
	calls := map[string]func() error{
		"Live":         func() error { _, err := r.Live(ctx, d); return err },
		"Member":       func() error { _, err := r.Member(ctx, d, "gm"); return err },
		"Members":      func() error { _, err := r.Members(ctx, d, Query{}); return err },
		"SetRole":      func() error { _, err := r.SetRole(ctx, d, "gm", RolePlayer); return err },
		"Invites":      func() error { _, err := r.Invites(ctx, d, Query{}); return err },
		"RevokeInvite": func() error { _, err := r.RevokeInvite(ctx, d, id); return err },
		"Requests":     func() error { _, err := r.Requests(ctx, d, "", Query{}); return err },
		"Blocked":      func() error { _, err := r.Blocked(ctx, d, "gm"); return err },
		"Blocks":       func() error { _, err := r.Blocks(ctx, d, Query{}); return err },
		"AddMember":    func() error { _, err := r.AddMember(ctx, d, "u1", RolePlayer); return err },
		"AddInvite":    func() error { _, err := r.AddInvite(ctx, d, "h", clock); return err },
		"Decide":       func() error { _, err := r.Decide(ctx, d, id, RequestRejected, "", "x"); return err },
		"RemoveMember": func() error { return r.RemoveMember(ctx, d, "gm") },
		"DropRequest":  func() error { return r.DropRequest(ctx, d, "gm") },
		"AddBlock":     func() error { return r.AddBlock(ctx, d, "u1") },
		"RemoveBlock":  func() error { return r.RemoveBlock(ctx, d, "u1") },
		"DropSession":  func() error { return r.DropSession(ctx, d) },
		"OwnerLimits":  func() error { return r.RecordOwnerLimits(ctx, d, nil) },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s with no decision = %v, want ErrForbidden", name, err)
		}
	}
}

type fixedGate struct {
	allow bool
	err   error
}

func (g fixedGate) Check(context.Context, string, string, string, string) (bool, error) {
	return g.allow, g.err
}

func TestAuthorizeFailsClosed(t *testing.T) {
	_, id := newSessionRepo(t)
	tests := []struct {
		name string
		ctx  context.Context
		sid  string
		want error
	}{
		{"no gate", ctx, id, relations.ErrUnavailable},
		{"auth-api down", WithGate(ctx, fixedGate{err: relations.ErrUnavailable}, "t"), id, relations.ErrUnavailable},
		{"token rejected", WithGate(ctx, fixedGate{err: relations.ErrUnauthorized}, "t"), id, relations.ErrUnauthorized},
		{"denied", WithGate(ctx, fixedGate{}, "t"), id, ErrForbidden},
		{"malformed session id", WithGate(ctx, fixedGate{allow: true}, "t"), "junk", ErrNotFound},
	}
	for _, tc := range tests {
		d, err := Authorize(tc.ctx, "gm", tc.sid, "can_view")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if d.require("can_view") == nil {
			t.Errorf("%s: got a usable decision", tc.name)
		}
	}
}

func TestRevokeInviteRecordsTheRevoker(t *testing.T) {
	r, id := newSessionRepo(t)
	in, err := r.AddInvite(ctx, dc(t, "gm", id), "hash", clock.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.RevokeInvite(ctx, dc(t, "co", id), in.ID)

	if err != nil || got.RevokedBy != "co" || got.RevokedAt == nil {
		t.Fatalf("revoked = %+v, %v; want revoked_by co", got, err)
	}
}

func TestScanMembersSeesEverySessionIncludingDeleted(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, dc(t, "gm", id), "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, dc(t, "gm", id), "u1"); err != nil {
		t.Fatal(err)
	}
	page, err := r.ScanMembers(ctx, Query{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var deleted int
	for _, m := range page.Items {
		if m.DeletedAt != nil {
			deleted++
		}
	}
	if len(page.Items) != 1 || deleted != 1 {
		t.Fatalf("items=%d deleted=%d, want the one deleted member kept in the scan", len(page.Items), deleted)
	}
}

type allowAll struct{}

func (allowAll) Check(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

// dc mints a granted decision the way production does, against a checker that allows everything.
func dc(t *testing.T, actor, id string) Decision { return dcAs(t, actor, id, "gm") }

// dcAs mints a decision for a specific relation.
func dcAs(t *testing.T, actor, id, relation string) Decision {
	t.Helper()
	d, err := Authorize(WithGate(ctx, allowAll{}, "tok"), actor, id, relation)
	if err != nil {
		t.Fatalf("authorize %s on %s: %v", actor, id, err)
	}
	return d
}

// ticketFor makes an invite in session id and resolves it for user, the way a join does.
func ticketFor(t *testing.T, r *Sessions, id, user string) JoinTicket {
	t.Helper()
	hash := "h-" + user + newID()
	if _, err := r.AddInvite(ctx, dc(t, "gm", id), hash, r.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tk, err := r.ResolveInvite(ctx, user, hash, r.now())
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestEachWriteRequiresItsRelation(t *testing.T) {
	r, id := newSessionRepo(t)
	view := dcAs(t, "obs", id, "can_view")
	edit := dcAs(t, "co", id, "can_edit")
	calls := map[string]struct {
		weak Decision
		call func(Decision) error
	}{
		"AddMember":    {view, func(d Decision) error { _, err := r.AddMember(ctx, d, "u1", RolePlayer); return err }},
		"Decide":       {view, func(d Decision) error { _, err := r.Decide(ctx, d, id, RequestRejected, "", "x"); return err }},
		"SetRole":      {view, func(d Decision) error { _, err := r.SetRole(ctx, d, "gm", RolePlayer); return err }},
		"RemoveOther":  {view, func(d Decision) error { return r.RemoveMember(ctx, d, "gm") }},
		"AddInvite":    {view, func(d Decision) error { _, err := r.AddInvite(ctx, d, "h", clock); return err }},
		"RevokeInvite": {view, func(d Decision) error { _, err := r.RevokeInvite(ctx, d, id); return err }},
		"Invites":      {view, func(d Decision) error { _, err := r.Invites(ctx, d, Query{}); return err }},
		"Requests":     {view, func(d Decision) error { _, err := r.Requests(ctx, d, "", Query{}); return err }},
		"Blocks":       {view, func(d Decision) error { _, err := r.Blocks(ctx, d, Query{}); return err }},
		"AddBlock":     {view, func(d Decision) error { return r.AddBlock(ctx, d, "u1") }},
		"RemoveBlock":  {view, func(d Decision) error { return r.RemoveBlock(ctx, d, "u1") }},
		"DropRequest":  {view, func(d Decision) error { return r.DropRequest(ctx, d, "u1") }},
		"Blocked":      {view, func(d Decision) error { _, err := r.Blocked(ctx, d, "u1"); return err }},
		"DropSession":  {edit, func(d Decision) error { return r.DropSession(ctx, d) }},
	}
	for name, tc := range calls {
		if err := tc.call(tc.weak); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s with a %s decision = %v, want ErrForbidden", name, tc.weak.relation, err)
		}
	}
	if page, _ := r.Members(ctx, dc(t, "gm", id), Query{}); len(page.Items) != 0 {
		t.Errorf("a refused write changed the members: %+v", page.Items)
	}
}

func TestLeavingNeedsOnlyView(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, dc(t, "gm", id), "obs", RoleObserver); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, dcAs(t, "obs", id, "can_view"), "obs"); err != nil {
		t.Fatalf("leave with can_view = %v, want success", err)
	}
}

func TestJoinCallsRefuseTheZeroTicket(t *testing.T) {
	r, _ := newSessionRepo(t)
	if _, err := r.ForJoin(ctx, JoinTicket{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("ForJoin = %v, want ErrForbidden", err)
	}
	if _, err := r.RequestJoin(ctx, JoinTicket{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("RequestJoin = %v, want ErrForbidden", err)
	}
}

func TestResolveInviteRefusesRevokedAndExpired(t *testing.T) {
	r, id := newSessionRepo(t)
	gm := dc(t, "gm", id)
	live, _ := r.AddInvite(ctx, gm, "live", r.now().Add(time.Hour))
	if _, err := r.AddInvite(ctx, gm, "old", r.now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeInvite(ctx, gm, live.ID); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"live", "old", "nope", ""} {
		if _, err := r.ResolveInvite(ctx, "u1", h, r.now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveInvite(%q) = %v, want ErrNotFound", h, err)
		}
	}
}
