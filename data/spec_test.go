package data

import (
	"context"
	"testing"

	"github.com/sweetrpg/initiative-api/store/fake"
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

func TestMemberIndexesLetSessionsWithGMUseASessionIDPrefix(t *testing.T) {
	for _, ix := range SessionMemberSpec.Indexes {
		if ix.Name == "session_user" && ix.Keys[0].Key == "session_id" {
			return
		}
	}
	t.Fatal("members has no index led by session_id; the reconciler's $in query would scan")
}
