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
	if _, err := r.AddMember(ctx, id, "u1", RolePlayer, "gm"); err != nil {
		t.Fatal(err)
	}

	if _, err := r.AddMember(ctx, id, "u1", RoleObserver, "gm"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v, want ErrConflict", err)
	}
	if _, err := r.AddMember(ctx, "junk", "u1", RolePlayer, "gm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad session id = %v, want ErrNotFound", err)
	}
	if _, err := r.AddMember(ctx, id, `{"$gt":""}`, RolePlayer, "gm"); !errors.Is(err, ErrInvalid) {
		t.Errorf("operator-shaped user = %v, want ErrInvalid", err)
	}
	if m, err := r.Member(ctx, id, "u1"); err != nil || m.Role != RolePlayer {
		t.Errorf("member = %+v, %v", m, err)
	}
}

func TestSetRoleAndRemoveMemberReportMissingMembers(t *testing.T) {
	r, id := newSessionRepo(t)
	if _, err := r.SetRole(ctx, id, "ghost", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role = %v, want ErrNotFound", err)
	}
	if err := r.RemoveMember(ctx, id, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove = %v, want ErrNotFound", err)
	}
	if _, err := r.SetRole(ctx, "junk", "u", RolePlayer); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role bad session = %v, want ErrNotFound", err)
	}
}

func TestBlocksAreIdempotentAndLiftable(t *testing.T) {
	r, id := newSessionRepo(t)

	for range 2 {
		if err := r.AddBlock(ctx, id, "bad", "gm"); err != nil {
			t.Fatal(err)
		}
	}

	if blocked, err := r.Blocked(ctx, id, "bad"); err != nil || !blocked {
		t.Fatalf("blocked = %v, %v", blocked, err)
	}
	if err := r.RemoveBlock(ctx, id, "bad"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveBlock(ctx, id, "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lift twice = %v, want ErrNotFound", err)
	}
	if err := r.AddBlock(ctx, "junk", "bad", "gm"); !errors.Is(err, ErrNotFound) {
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
	if _, err := r.Decide(ctx, id, first.ID, RequestRejected, "", "no", "gm"); err != nil {
		t.Fatal(err)
	}

	again, err := r.RequestJoin(ctx, id, "u1")

	if err != nil || again.Status != RequestPending || again.Reason != "" || again.DecidedBy != "" {
		t.Fatalf("after rejection = %+v, %v; want a clean pending request", again, err)
	}
	if _, err := r.Decide(ctx, id, "junk", RequestApproved, RolePlayer, "", "gm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("junk request id = %v, want ErrNotFound", err)
	}
	if _, err := r.Decide(ctx, id, testID("z"), RequestApproved, RolePlayer, "", "gm"); !errors.Is(err, ErrNotFound) {
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
	if _, err := r.AddMember(ctx, s.ID, "gm", RoleGM, "gm"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, s.ID, "u1", RolePlayer, "gm"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddInvite(ctx, s.ID, "hash", clock.Add(time.Hour), "gm"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RequestJoin(ctx, s.ID, "u2"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddBlock(ctx, s.ID, "u3", "gm"); err != nil {
		t.Fatal(err)
	}

	if err := r.DropSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	for _, c := range []string{CollectionSessionMembers, CollectionSessionInvites, CollectionSessionRequests, CollectionSessionBlocks} {
		if n := len(db.Fake(c).Docs()); n != 0 {
			t.Errorf("%s still holds %d records", c, n)
		}
	}
	if err := r.DropSession(ctx, "junk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("drop bad id = %v, want ErrNotFound", err)
	}
}
