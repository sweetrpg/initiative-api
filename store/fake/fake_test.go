package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

type doc struct {
	ID   string   `bson:"_id"`
	Name string   `bson:"name"`
	N    int      `bson:"n"`
	Tags []string `bson:"tags"`
}

func seeded(t *testing.T) *Collection {
	t.Helper()
	c := NewDB().Fake("c")
	for _, d := range []doc{{"a", "alpha", 1, []string{"x"}}, {"b", "beta", 2, nil}, {"c", "gamma", 3, []string{"y"}}} {
		if err := c.InsertOne(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestFindFilters(t *testing.T) {
	tests := []struct {
		name   string
		filter bson.D
		want   int
	}{
		{"equality", bson.D{{Key: "name", Value: "beta"}}, 1},
		{"gt", bson.D{{Key: "n", Value: bson.D{{Key: "$gt", Value: 1}}}}, 2},
		{"in", bson.D{{Key: "name", Value: bson.D{{Key: "$in", Value: bson.A{"alpha", "gamma"}}}}}, 2},
		{"regex ignore case", bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^AL"}, {Key: "$options", Value: "i"}}}}, 1},
		{"or", bson.D{{Key: "$or", Value: bson.A{bson.D{{Key: "n", Value: 1}}, bson.D{{Key: "n", Value: 3}}}}}, 2},
		{"nil matches null field", bson.D{{Key: "tags", Value: nil}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out []doc
			if err := seeded(t).Find(context.Background(), tt.filter, store.FindOptions{}, &out); err != nil {
				t.Fatal(err)
			}
			if len(out) != tt.want {
				t.Fatalf("got %d docs, want %d", len(out), tt.want)
			}
		})
	}
}

func TestFindSortAndLimit(t *testing.T) {
	var out []doc
	err := seeded(t).Find(context.Background(), bson.D{}, store.FindOptions{Sort: bson.D{{Key: "n", Value: -1}}, Limit: 2}, &out)
	if err != nil || len(out) != 2 || out[0].ID != "c" {
		t.Fatalf("out=%v err=%v, want [c b]", out, err)
	}
}

func TestInsertDuplicateID(t *testing.T) {
	if err := seeded(t).InsertOne(context.Background(), doc{ID: "a"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestUniqueIndexEnforced(t *testing.T) {
	c := seeded(t)
	_ = c.EnsureIndexes(context.Background(), []store.IndexSpec{{Name: "n", Keys: bson.D{{Key: "n", Value: 1}}, Unique: true}})
	if err := c.InsertOne(context.Background(), doc{ID: "z", N: 1}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestFindOneAndUpdateSetIncUnset(t *testing.T) {
	var out doc
	upd := bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "ALPHA"}}}, {Key: "$inc", Value: bson.D{{Key: "n", Value: 5}}}, {Key: "$unset", Value: bson.D{{Key: "tags", Value: ""}}}}
	if err := seeded(t).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: "a"}}, upd, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "ALPHA" || out.N != 6 || out.Tags != nil {
		t.Fatalf("out = %+v", out)
	}
}

func TestFindOneNotFound(t *testing.T) {
	var out doc
	if err := seeded(t).FindOne(context.Background(), bson.D{{Key: "_id", Value: "nope"}}, &out); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteOneRemovesFirstMatchOnly(t *testing.T) {
	c := NewDB().Fake("c")
	for _, id := range []string{"a", "b"} {
		if err := c.InsertOne(context.Background(), bson.M{"_id": id, "k": 1}); err != nil {
			t.Fatal(err)
		}
	}

	err := c.DeleteOne(context.Background(), bson.D{{Key: "k", Value: 1}})
	missing := c.DeleteOne(context.Background(), bson.D{{Key: "k", Value: 9}})

	if err != nil || len(c.Docs()) != 1 || c.Docs()[0]["_id"] != "b" {
		t.Fatalf("err = %v, docs = %v", err, c.Docs())
	}
	if !errors.Is(missing, store.ErrNotFound) {
		t.Fatalf("missing = %v, want ErrNotFound", missing)
	}
}

func TestEmptyAndIsRejected(t *testing.T) {
	c := seeded(t)
	for _, key := range []string{"$and", "$or"} {
		var out []doc
		err := c.Find(context.Background(), bson.D{{Key: key, Value: bson.A{}}}, store.FindOptions{}, &out)
		if err == nil {
			t.Errorf("empty %s accepted; MongoDB rejects it", key)
		}
	}
}
