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
