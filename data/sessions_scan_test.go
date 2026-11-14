package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

type failingUpdates struct {
	store.Collection
	fail bool
}

func (f *failingUpdates) FindOneAndUpdate(ctx context.Context, filter, update bson.D, out any) error {
	if f.fail {
		return errors.New("mongo down")
	}
	return f.Collection.FindOneAndUpdate(ctx, filter, update, out)
}

func TestRemoveGMLessReapsASessionWhoseOnlyGMRecordIsSoftDeleted(t *testing.T) {
	db := fake.NewDB()
	r := NewSessions(db)
	r.now = func() time.Time { return time.Now().Add(time.Hour) }
	s := &Session{Name: "Stripped"}
	if err := r.owned.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, dc(t, "gm", s.ID), "gm", RoleGM); err != nil {
		t.Fatal(err)
	}
	if err := r.softDelete(ctx, r.members, sessionUser(s.ID, "gm"), "operator"); err != nil {
		t.Fatal(err)
	}

	if err := r.RemoveGMLess(ctx, s.ID); err != nil {
		t.Fatalf("remove = %v, want the session reaped", err)
	}
	if n, _ := r.Count(ctx, "gm"); n != 0 {
		t.Errorf("owned sessions = %d, want 0", n)
	}
}

func TestRemoveGMLessFinishesOnTheNextPassAfterADropFailsMidway(t *testing.T) {
	db := fake.NewDB()
	r := NewSessions(db)
	r.now = func() time.Time { return time.Now().Add(time.Hour) }
	s := &Session{Name: "Orphan"}
	if err := r.owned.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, dc(t, "gm", s.ID), "p1", RolePlayer); err != nil {
		t.Fatal(err)
	}
	flaky := &failingUpdates{Collection: r.requests, fail: true}
	r.requests = flaky

	if err := r.RemoveGMLess(ctx, s.ID); err == nil {
		t.Fatal("remove = nil, want the failed drop reported")
	}
	if n, _ := r.Count(ctx, "gm"); n != 1 {
		t.Fatalf("owned sessions = %d, want the session still live so the next pass finds it", n)
	}
	flaky.fail = false

	if err := r.RemoveGMLess(ctx, s.ID); err != nil {
		t.Fatalf("second pass = %v", err)
	}
	if n, _ := r.Count(ctx, "gm"); n != 0 {
		t.Errorf("owned sessions = %d, want 0 after the next pass", n)
	}
	for _, d := range db.Fake(CollectionSessionMembers).Docs() {
		if d["deleted_at"] == nil {
			t.Errorf("member %v left live", d)
		}
	}
}

func TestRemoveGMLessReapsAStrippedGMsSessionAndDropsItsOtherMembers(t *testing.T) {
	db := fake.NewDB()
	r := NewSessions(db)
	r.now = func() time.Time { return time.Now().Add(time.Hour) }
	s := &Session{Name: "Stripped"}
	if err := r.owned.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ u, role string }{{"gm", RoleGM}, {"p1", RolePlayer}} {
		if _, err := r.AddMember(ctx, dc(t, "gm", s.ID), m.u, m.role); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.softDelete(ctx, r.members, sessionUser(s.ID, "gm"), "operator"); err != nil {
		t.Fatal(err)
	}

	if err := r.RemoveGMLess(ctx, s.ID); err != nil {
		t.Fatalf("remove = %v", err)
	}
	for _, d := range db.Fake(CollectionSessionMembers).Docs() {
		if d["deleted_at"] == nil {
			t.Errorf("member %v left live", d)
		}
	}
}

func TestRemoveGMLessRefusesASessionYoungerThanTheMintAge(t *testing.T) {
	db := fake.NewDB()
	r := NewSessions(db)
	s := &Session{Name: "New"}
	if err := r.owned.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}

	if err := r.RemoveGMLess(ctx, s.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove = %v, want ErrConflict for a session younger than %v", err, gmLessMinAge)
	}
}
