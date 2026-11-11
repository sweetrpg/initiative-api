//go:build integration

package data

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Run with: MONGODB_TEST_URI=mongodb://localhost:27017 go test -tags integration ./data/
// Proves against a real server that exactly one of N concurrent revision-1 writers wins.
func TestMongoConcurrentWritersExactlyOneWins(t *testing.T) {
	uri := os.Getenv("MONGODB_TEST_URI")
	if uri == "" {
		t.Skip("MONGODB_TEST_URI not set")
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	db := client.Database("initiative_api_test")
	t.Cleanup(func() { _ = db.Drop(ctx) })

	r := NewOwned[Roster](store.MongoDatabase{DB: db}, RosterSpec)
	d := &Roster{Name: "Goblins"}
	if err := r.Create(ctx, "u1", "u1", d); err != nil {
		t.Fatal(err)
	}
	const writers = 20
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.Update(ctx, "u1", "u1", d.ID, 1, map[string]any{"name": "w"})
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want 1", wins)
	}
}

// Proves the $expr/$getField lookup against a real server, including a dotted key and owner
// scoping, which the in-memory fake cannot evaluate.
func TestMongoFindByExternalRef(t *testing.T) {
	uri := os.Getenv("MONGODB_TEST_URI")
	if uri == "" {
		t.Skip("MONGODB_TEST_URI not set")
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	db := client.Database("initiative_api_refs_test")
	t.Cleanup(func() { _ = db.Drop(ctx) })

	encs := NewEncounters(store.MongoDatabase{DB: db})
	mk := func(owner string, removed bool) {
		e := &Encounter{Name: "e", Ordering: OrderHighToLow, TieBreaker: TieRandom, Status: StatusDraft,
			Combatants: []Combatant{{ID: "c1", Name: "Orc", Kind: KindAdversary, Removed: removed,
				ExternalRefs: map[string]string{"foundry.actor": "abc"}}, {ID: "c2", Name: "Elf", Kind: KindPC}}}
		if err := encs.Owned.Create(ctx, owner, owner, e); err != nil {
			t.Fatal(err)
		}
	}
	mk("u1", false)
	mk("u1", true)
	mk("u2", false)
	got, err := encs.FindByExternalRef(ctx, "u1", "foundry.actor", "abc")
	if err != nil || len(got) != 1 || got[0].Combatant.ID != "c1" {
		t.Fatalf("got %+v err=%v, want u1's one live match", got, err)
	}
	if got, _ := encs.FindByExternalRef(ctx, "u1", "foundry.actor", "zzz"); len(got) != 0 {
		t.Fatalf("non-matching value returned %+v", got)
	}
}
