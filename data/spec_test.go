package data

import (
	"context"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

func TestEnsureIndexesCoversEveryCollection(t *testing.T) {
	db := fake.NewDB()
	if err := EnsureIndexes(context.Background(), db); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	for _, s := range AllSpecs() {
		t.Run(s.Name, func(t *testing.T) {
			if got := len(db.Fake(s.Name).Indexes); got != len(s.Indexes) || got == 0 {
				t.Fatalf("indexes on %s = %d, want %d (non-zero)", s.Name, got, len(s.Indexes))
			}
		})
	}
}

func TestOwnerScopedIndexesLeadWithOwner(t *testing.T) {
	for _, s := range []Spec{RosterSpec, EncounterSpec, ThemeSpec, CredentialSpec} {
		for _, ix := range s.Indexes {
			if ix.Name == "secret_hash" { // global lookup by presented secret
				continue
			}
			t.Run(s.Name+"/"+ix.Name, func(t *testing.T) {
				if ix.Keys[0].Key != "owner_user_id" {
					t.Fatalf("first key = %q, want owner_user_id", ix.Keys[0].Key)
				}
			})
		}
	}
}

func TestSessionMemberIndexMatchesTheGMLookupFilters(t *testing.T) {
	for _, ix := range SessionMemberSpec.Indexes {
		if ix.Name != "session_user" {
			continue
		}
		if ix.Keys[0].Key != "session_id" || len(ix.Partial) != 1 || ix.Partial[0].Key != "live" {
			t.Fatalf("session_user = keys %v partial %v; the GM lookups filter on session_id and live $exists", ix.Keys, ix.Partial)
		}
		return
	}
	t.Fatal("members has no session_user index")
}

// spyFilters records every filter a collection is queried with.
type spyFilters struct {
	store.Collection
	filters []bson.D
}

func (s *spyFilters) Find(ctx context.Context, f bson.D, o store.FindOptions, out any) error {
	s.filters = append(s.filters, f)
	return s.Collection.Find(ctx, f, o, out)
}

func (s *spyFilters) FindOne(ctx context.Context, f bson.D, out any) error {
	s.filters = append(s.filters, f)
	return s.Collection.FindOne(ctx, f, out)
}

func TestBothGMLookupsFilterOnTheLivePredicateTheIndexIsPartialOn(t *testing.T) {
	r := NewSessions(fake.NewDB())
	r.now = func() time.Time { return time.Now().Add(time.Hour) }
	spy := &spyFilters{Collection: r.members}
	r.members = spy
	s := &Session{Name: "S"}
	if err := r.owned.Create(ctx, "gm", "gm", s); err != nil {
		t.Fatal(err)
	}

	if _, err := r.SessionsWithGM(ctx, []string{s.ID}); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveGMLess(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	gmFilters := 0
	for _, f := range spy.filters {
		if !hasKey(f, "role") {
			continue
		}
		gmFilters++
		if !hasKey(f, "live") || hasKey(f, "deleted_at") {
			t.Errorf("GM filter %v must use live $exists, not deleted_at", f)
		}
	}
	if gmFilters != 2 {
		t.Fatalf("saw %d GM filters, want 2 (SessionsWithGM and RemoveGMLess)", gmFilters)
	}
}

func hasKey(f bson.D, k string) bool {
	for _, e := range f {
		if e.Key == k {
			return true
		}
	}
	return false
}
