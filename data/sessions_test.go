package data

import (
	"errors"
	"testing"
	"time"

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
	if _, err := r.AddMember(ctx, "gm", id, "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}

	if _, err := r.AddMember(ctx, "gm", id, "u1", RoleObserver); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v, want ErrConflict", err)
	}
	if _, err := r.AddMember(ctx, "gm", "junk", "u1", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad session id = %v, want ErrNotFound", err)
	}
	if _, err := r.AddMember(ctx, "gm", id, `{"$gt":""}`, RolePlayer); !errors.Is(err, ErrInvalid) {
		t.Errorf("operator-shaped user = %v, want ErrInvalid", err)
	}
	if m, err := r.Member(ctx, "gm", id, "u1"); err != nil || m.Role != RolePlayer {
		t.Errorf("member = %+v, %v", m, err)
	}
}

func TestSetRoleAndRemoveMemberReportMissingMembers(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.SetRole(ctx, "gm", id, "ghost", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role = %v, want ErrNotFound", err)
	}
	if err := r.RemoveMember(ctx, "gm", id, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove = %v, want ErrNotFound", err)
	}
	if _, err := r.SetRole(ctx, "gm", "junk", "u", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role bad session = %v, want ErrNotFound", err)
	}
}

func TestBlocksAreIdempotentAndLiftable(t *testing.T) {
	r, id := newSessionRepo(t)

	for range 2 {
		if err := r.AddBlock(ctx, "gm", id, "bad"); err != nil {
			t.Fatal(err)
		}
	}

	if blocked, err := r.Blocked(ctx, "gm", id, "bad"); err != nil || !blocked {
		t.Fatalf("blocked = %v, %v", blocked, err)
	}
	if err := r.RemoveBlock(ctx, "gm", id, "bad"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveBlock(ctx, "gm", id, "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lift twice = %v, want ErrNotFound", err)
	}
	if err := r.AddBlock(ctx, "gm", "junk", "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("block bad session = %v, want ErrNotFound", err)
	}
}

func TestRequestJoinStates(t *testing.T) {
	r, id := newSessionRepo(t)
	first, err := r.RequestJoin(ctx, id, "u1")
	if err != nil || first.Status != RequestPending {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if same, _ := r.RequestJoin(ctx, id, "u1"); same.ID != first.ID {
		t.Fatalf("second request got a new id")
	}
	if _, err := r.Decide(ctx, "gm", id, first.ID, RequestRejected, "", "no"); err != nil {
		t.Fatal(err)
	}

	again, err := r.RequestJoin(ctx, id, "u1")

	if err != nil || again.Status != RequestPending || again.Reason != "" || again.DecidedBy != "" {
		t.Fatalf("after rejection = %+v, %v; want a clean pending request", again, err)
	}
	if _, err := r.Decide(ctx, "gm", id, "junk", RequestApproved, RolePlayer, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("junk request id = %v, want ErrNotFound", err)
	}
	if _, err := r.Decide(ctx, "gm", id, testID("z"), RequestApproved, RolePlayer, ""); !errors.Is(err, ErrNotFound) {
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
	if _, err := r.AddMember(ctx, "gm", s.ID, "gm", RoleGM); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, "gm", s.ID, "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddInvite(ctx, "gm", s.ID, "hash", clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RequestJoin(ctx, s.ID, "u2"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddBlock(ctx, "gm", s.ID, "u3"); err != nil {
		t.Fatal(err)
	}

	if err := r.DropSession(ctx, "gm", s.ID); err != nil {
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
	if _, err := r.Member(ctx, "gm", s.ID, "u1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped member still readable: %v", err)
	}
	if _, err := r.InviteByHash(ctx, "gm", "hash"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped invite still resolves: %v", err)
	}
	if err := r.DropSession(ctx, "gm", "junk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("drop bad id = %v, want ErrNotFound", err)
	}
}

func TestRemovedMemberCanBeAddedAgain(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, "gm", id, "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, "gm", id, "u1"); err != nil {
		t.Fatal(err)
	}

	m, err := r.AddMember(ctx, "gm", id, "u1", RoleObserver)

	if err != nil || m.Role != RoleObserver {
		t.Fatalf("re-add = %+v, %v; the soft-deleted row must not block it", m, err)
	}
	if page, _ := r.Members(ctx, "gm", id, Query{}); len(page.Items) != 1 {
		t.Fatalf("members = %d, want only the live one", len(page.Items))
	}
}

func TestRemovedMemberIsNotListedUnderTheirSessions(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, "gm", id, "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, "gm", id, "u1"); err != nil {
		t.Fatal(err)
	}

	page, err := r.SessionIDsOf(ctx, "u1", Query{})

	if err != nil || len(page.Items) != 0 {
		t.Fatalf("sessions of removed member = %+v, %v; want none", page.Items, err)
	}
}

func TestEveryDataCallRefusesNoActor(t *testing.T) {
	r, id := newSessionRepo(t)
	calls := map[string]func() error{
		"Live":         func() error { _, err := r.Live(ctx, "", id); return err },
		"Member":       func() error { _, err := r.Member(ctx, "", id, "gm"); return err },
		"Members":      func() error { _, err := r.Members(ctx, "", id, Query{}); return err },
		"SetRole":      func() error { _, err := r.SetRole(ctx, "", id, "gm", RolePlayer); return err },
		"InviteByHash": func() error { _, err := r.InviteByHash(ctx, "", "h"); return err },
		"Invites":      func() error { _, err := r.Invites(ctx, "", id, Query{}); return err },
		"RevokeInvite": func() error { _, err := r.RevokeInvite(ctx, "", id, id); return err },
		"Requests":     func() error { _, err := r.Requests(ctx, "", id, "", Query{}); return err },
		"Blocked":      func() error { _, err := r.Blocked(ctx, "", id, "gm"); return err },
		"Blocks":       func() error { _, err := r.Blocks(ctx, "", id, Query{}); return err },
		"AddMember":    func() error { _, err := r.AddMember(ctx, "", id, "u1", RolePlayer); return err },
		"AddInvite":    func() error { _, err := r.AddInvite(ctx, "", id, "h", clock); return err },
		"Decide":       func() error { _, err := r.Decide(ctx, "", id, id, RequestRejected, "", "x"); return err },
		"RemoveMember": func() error { return r.RemoveMember(ctx, "", id, "gm") },
		"DropRequest":  func() error { return r.DropRequest(ctx, "", id, "gm") },
		"AddBlock":     func() error { return r.AddBlock(ctx, "", id, "u1") },
		"RemoveBlock":  func() error { return r.RemoveBlock(ctx, "", id, "u1") },
		"DropSession":  func() error { return r.DropSession(ctx, "", id) },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s with no actor = %v, want ErrInvalid", name, err)
		}
	}
}

func TestRevokeInviteRecordsTheRevoker(t *testing.T) {
	r, id := newSessionRepo(t)
	in, err := r.AddInvite(ctx, "gm", id, "hash", clock.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.RevokeInvite(ctx, "co", id, in.ID)

	if err != nil || got.RevokedBy != "co" || got.RevokedAt == nil {
		t.Fatalf("revoked = %+v, %v; want revoked_by co", got, err)
	}
}

func TestScanMembersSeesEverySessionIncludingDeleted(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.AddMember(ctx, "gm", id, "u1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, "gm", id, "u1"); err != nil {
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
