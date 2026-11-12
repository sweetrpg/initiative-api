package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// ItemState is what limit enforcement needs to know about one stored item.
type ItemState struct {
	ID        string
	CreatedAt time.Time
	KeepRank  int
	Inactive  bool
}

// ItemStates lists owner's live items oldest first, for deciding which stay active.
func (r *Owned[T, P]) ItemStates(ctx context.Context, owner string) ([]ItemState, error) {
	return r.itemStates(ctx, scope(owner))
}

func (r *Owned[T, P]) itemStates(ctx context.Context, filter bson.D) ([]ItemState, error) {
	var docs []T
	opts := store.FindOptions{
		Sort:       bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}},
		Projection: bson.D{{Key: "created_at", Value: 1}, {Key: "keep_rank", Value: 1}, {Key: "inactive_at", Value: 1}},
	}
	if err := r.c.Find(ctx, filter, opts, &docs); err != nil {
		return nil, err
	}
	out := make([]ItemState, 0, len(docs))
	for i := range docs {
		m := P(&docs[i]).GetMeta()
		out = append(out, ItemState{ID: m.ID, CreatedAt: m.CreatedAt, KeepRank: m.KeepRank, Inactive: m.InactiveAt != nil})
	}
	return out, nil
}

// SetInactive marks owner's item inactive or active. It changes neither the revision nor the
// audit stamp: the owner did not edit the item.
func (r *Owned[T, P]) SetInactive(ctx context.Context, owner, id string, inactive bool) error {
	id, ok := cleanID(id)
	if !ok {
		return ErrNotFound
	}
	var update bson.D
	if inactive {
		update = bson.D{{Key: "$set", Value: bson.D{{Key: "inactive_at", Value: r.now()}}}}
	} else {
		update = bson.D{{Key: "$unset", Value: bson.D{{Key: "inactive_at", Value: ""}}}}
	}
	var discard T
	err := r.c.FindOneAndUpdate(ctx, scopeID(owner, id), update, &discard)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// SetKeepRanks records the owner's choice: ids get ranks 1..n in order, every other item loses
// its rank. Items already in that state are not written.
func (r *Owned[T, P]) SetKeepRanks(ctx context.Context, owner string, ids []string) error {
	states, err := r.ItemStates(ctx, owner)
	if err != nil {
		return err
	}
	want := make(map[string]int, len(ids))
	for i, id := range ids {
		want[id] = i + 1
	}
	for _, st := range states {
		if st.KeepRank == want[st.ID] {
			continue
		}
		update := bson.D{{Key: "$unset", Value: bson.D{{Key: "keep_rank", Value: ""}}}}
		if n := want[st.ID]; n > 0 {
			update = bson.D{{Key: "$set", Value: bson.D{{Key: "keep_rank", Value: n}}}}
		}
		var discard T
		if err := r.c.FindOneAndUpdate(ctx, scopeID(owner, st.ID), update, &discard); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("set keep rank: %w", err)
		}
	}
	return nil
}
