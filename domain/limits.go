package domain

import (
	"context"
	"log/slog"
	"slices"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// Entitlement keys for the stored-item limits.
const (
	KeySavedRosters    = "initiative.saved_rosters"
	KeySavedEncounters = "initiative.saved_encounters"
)

// Limited is a repository whose items can go inactive when the owner's limit falls.
type Limited interface {
	ItemStates(ctx context.Context, owner string) ([]data.ItemState, error)
	SetInactive(ctx context.Context, owner, id string, inactive bool) error
	SetKeepRanks(ctx context.Context, owner string, ids []string) error
}

// Kind is one limited item type: its URL name, its entitlement key, and its items.
type Kind struct {
	Name string
	Key  string
	Repo Limited
}

// Limits keeps only as many of an owner's items active as the entitled limit allows (spec
// initiative-entitlement-gating). It never deletes or archives anything.
type Limits struct {
	kinds []Kind
}

// NewLimits builds the enforcer for kinds.
func NewLimits(kinds ...Kind) *Limits { return &Limits{kinds: kinds} }

// plan returns the IDs to activate and to deactivate so that exactly the first limit items in
// preference order are active: the owner's choice (by rank) first, then oldest first. states must
// arrive oldest first.
func plan(states []data.ItemState, limit int) (activate, deactivate []string) {
	ordered := slices.Clone(states)
	slices.SortStableFunc(ordered, func(a, b data.ItemState) int {
		switch {
		case a.KeepRank > 0 && b.KeepRank > 0:
			return a.KeepRank - b.KeepRank
		case a.KeepRank > 0:
			return -1
		case b.KeepRank > 0:
			return 1
		}
		return 0
	})
	for i, st := range ordered {
		switch {
		case i < limit && st.Inactive:
			activate = append(activate, st.ID)
		case i >= limit && !st.Inactive:
			deactivate = append(deactivate, st.ID)
		}
	}
	return activate, deactivate
}

func (l *Limits) kind(name string) (Kind, bool) {
	for _, k := range l.kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

func (l *Limits) apply(ctx context.Context, owner string, k Kind, limit int) error {
	states, err := k.Repo.ItemStates(ctx, owner)
	if err != nil {
		return err
	}
	activate, deactivate := plan(states, limit)
	for _, id := range deactivate {
		if err := k.Repo.SetInactive(ctx, owner, id, true); err != nil {
			return err
		}
	}
	for _, id := range activate {
		if err := k.Repo.SetInactive(ctx, owner, id, false); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile brings every kind in line with owner's current limits. When entitlements cannot be
// read it leaves that kind as it is, so an outage never deactivates anything. It tries every
// kind and returns the first error.
func (l *Limits) Reconcile(ctx context.Context, owner string, chk Checker) error {
	var first error
	for _, k := range l.kinds {
		res, err := chk.Check(ctx, k.Key, entitlements.FailOpen)
		if err == nil && res.Degraded {
			continue
		}
		if err == nil {
			err = l.apply(ctx, owner, k, res.Limit)
		}
		if err != nil {
			slog.WarnContext(ctx, "limit reconcile failed", "kind", k.Name, "error", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// Choose records which items of a kind the owner wants active, in order, then applies the limit.
// ids must be distinct items the owner holds, at most the limit; if fewer than the limit are
// chosen, the remaining places go to the oldest other items. Returns the active IDs afterward.
func (l *Limits) Choose(ctx context.Context, owner, kind string, ids []string, chk Checker) ([]string, error) {
	k, ok := l.kind(kind)
	if !ok {
		return nil, data.ErrNotFound
	}
	res, err := chk.Check(ctx, k.Key, entitlements.FailClosed)
	if err != nil {
		return nil, err
	}
	states, err := k.Repo.ItemStates(ctx, owner)
	if err != nil {
		return nil, err
	}
	if len(ids) > res.Limit {
		return nil, invalid("at most %d items can be active", res.Limit)
	}
	held := make(map[string]bool, len(states))
	for _, st := range states {
		held[st.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !held[id] || seen[id] {
			return nil, invalid("ids must be distinct items you own")
		}
		seen[id] = true
	}
	if err := k.Repo.SetKeepRanks(ctx, owner, ids); err != nil {
		return nil, err
	}
	if err := l.apply(ctx, owner, k, res.Limit); err != nil {
		return nil, err
	}
	after, err := k.Repo.ItemStates(ctx, owner)
	if err != nil {
		return nil, err
	}
	active := []string{}
	for _, st := range after {
		if !st.Inactive {
			active = append(active, st.ID)
		}
	}
	return active, nil
}
