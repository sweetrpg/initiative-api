package data

import (
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func newEncounters(t *testing.T) *Owned[Encounter, *Encounter] {
	t.Helper()
	r := NewOwned[Encounter](fake.NewDB(), EncounterSpec)
	n := 0
	r.newID = func() string { n++; return string(rune('a' + n - 1)) }
	r.now = func() time.Time { return clock }
	return r
}

func newEncounter(t *testing.T, r *Owned[Encounter, *Encounter], owner string) *Encounter {
	t.Helper()
	e := &Encounter{Name: "Ambush", Status: StatusDraft, Ordering: OrderHighToLow, TieBreaker: TieRandom}
	if err := r.Create(ctx, owner, owner, e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEncounterCreateStartsAtRevisionOneOwnedByCaller(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if e.OwnerUserID != "u1" || e.Revision != 1 {
		t.Fatalf("owner=%q revision=%d, want u1 and 1", e.OwnerUserID, e.Revision)
	}
}

func TestEncounterGetByOtherOwnerIsNotFound(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if _, err := r.Get(ctx, "u2", e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEncounterUpdateByOtherOwnerIsForbiddenAndChangesNothing(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	_, err := r.Update(ctx, "u2", "u2", e.ID, 1, map[string]any{"name": "Stolen"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	got, _ := r.Get(ctx, "u1", e.ID)
	if got.Name != "Ambush" || got.Revision != 1 {
		t.Fatalf("encounter changed: name=%q revision=%d", got.Name, got.Revision)
	}
}

func TestEncounterUpdateWithStaleRevisionReturnsCurrent(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if _, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"round": 1}); err != nil {
		t.Fatal(err)
	}
	cur, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"round": 2})
	if !errors.Is(err, ErrRevisionConflict) || cur.Revision != 2 || cur.Round != 1 {
		t.Fatalf("err=%v revision=%d round=%d, want conflict at revision 2 round 1", err, cur.Revision, cur.Round)
	}
}

func TestEncounterCombatStateIsOneRevisionedWrite(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	init := 14.0
	set := map[string]any{
		"status":               StatusActive,
		"round":                1,
		"current_combatant_id": "c1",
		"turn_queue":           []string{"c1"},
		"combatants":           []Combatant{{ID: "c1", Name: "Goblin", Kind: KindAdversary, Initiative: &init}},
	}
	got, err := r.Update(ctx, "u1", "u1", e.ID, 1, set)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Status != StatusActive || len(got.Combatants) != 1 || got.TurnQueue[0] != "c1" {
		t.Fatalf("got %+v, want one write at revision 2 with state and combatants", got)
	}
}

func TestEncounterSoftDeleteHidesIt(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if err := r.SoftDelete(ctx, "u1", "u1", e.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "u1", e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEncounterSpecHasNoSessionField(t *testing.T) {
	for _, f := range append(append(EncounterSpec.Updatable, EncounterSpec.Filterable...), EncounterSpec.Projectable...) {
		if f == "session_id" {
			t.Fatalf("session_id is exposed; sessions need the OpenFGA path (PADR-0035)")
		}
	}
}
