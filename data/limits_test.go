package data

import (
	"errors"
	"testing"
)

func TestSetInactiveFlipsOnlyTheOwnersItemAndKeepsRevision(t *testing.T) {
	r, _ := newRosters(t)
	a := mustCreate(t, r, "u1", "A")

	if err := r.SetInactive(ctx, "u1", a.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, "u1", a.ID)
	if got.InactiveAt == nil || got.Revision != 1 {
		t.Fatalf("after deactivate: inactive=%v revision=%d", got.InactiveAt, got.Revision)
	}
	if err := r.SetInactive(ctx, "u2", a.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner = %v, want ErrNotFound", err)
	}
	if err := r.SetInactive(ctx, "u1", "junk", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v, want ErrNotFound", err)
	}
	if err := r.SetInactive(ctx, "u1", a.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "u1", a.ID); got.InactiveAt != nil {
		t.Fatal("still inactive after reactivation")
	}
}

func TestItemStatesListOldestFirstWithRanks(t *testing.T) {
	r, _ := newRosters(t)
	a := mustCreate(t, r, "u1", "A")
	b := mustCreate(t, r, "u1", "B")
	mustCreate(t, r, "u2", "other")
	if err := r.SetKeepRanks(ctx, "u1", []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetInactive(ctx, "u1", a.ID, true); err != nil {
		t.Fatal(err)
	}

	states, err := r.ItemStates(ctx, "u1")

	if err != nil || len(states) != 2 || states[0].ID != a.ID || !states[0].Inactive || states[1].KeepRank != 1 {
		t.Fatalf("states = %+v, %v", states, err)
	}
}

func TestSetKeepRanksReplacesTheChoice(t *testing.T) {
	r, _ := newRosters(t)
	a := mustCreate(t, r, "u1", "A")
	b := mustCreate(t, r, "u1", "B")
	if err := r.SetKeepRanks(ctx, "u1", []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}

	if err := r.SetKeepRanks(ctx, "u1", []string{b.ID}); err != nil {
		t.Fatal(err)
	}

	states, _ := r.ItemStates(ctx, "u1")
	rank := map[string]int{states[0].ID: states[0].KeepRank, states[1].ID: states[1].KeepRank}
	if rank[a.ID] != 0 || rank[b.ID] != 1 {
		t.Fatalf("ranks = %v, want a unranked and b first", rank)
	}
}

func inSystem(t *testing.T, r *Encounters, owner, system string) *Encounter {
	t.Helper()
	e := &Encounter{Name: "Ambush", Status: StatusDraft, Ordering: OrderHighToLow, TieBreaker: TieRandom, GameSystemID: system}
	if err := r.Create(ctx, owner, owner, e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLockGatedLocksOnlyGatedSystemsAndUnlockRestores(t *testing.T) {
	r := newEncounters(t)
	free := inSystem(t, r, "u1", DefaultSystemID)
	gated := inSystem(t, r, "u1", "dnd5")
	other := inSystem(t, r, "u2", "dnd5")

	n, err := r.LockGated(ctx, "u1")

	if err != nil || n != 1 {
		t.Fatalf("locked %d, %v; want 1", n, err)
	}
	if _, err := r.Update(ctx, "u1", "u1", gated.ID, 1, map[string]any{"name": "x"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("edit locked = %v, want ErrLocked", err)
	}
	if _, err := r.Update(ctx, "u1", "u1", free.ID, 1, map[string]any{"name": "x"}); err != nil {
		t.Fatalf("edit free = %v", err)
	}
	if _, err := r.Update(ctx, "u2", "u2", other.ID, 1, map[string]any{"name": "x"}); err != nil {
		t.Fatalf("other owner's encounter was locked: %v", err)
	}
	if again, _ := r.LockGated(ctx, "u1"); again != 0 {
		t.Fatalf("second lock pass touched %d", again)
	}
	un, err := r.UnlockAll(ctx, "u1")
	if err != nil || un != 1 {
		t.Fatalf("unlocked %d, %v; want 1", un, err)
	}
	if _, err := r.Update(ctx, "u1", "u1", gated.ID, 1, map[string]any{"name": "back"}); err != nil {
		t.Fatalf("edit after unlock = %v", err)
	}
}
