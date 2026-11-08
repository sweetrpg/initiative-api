package data

import (
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func newEncounters(t *testing.T) *Encounters {
	t.Helper()
	r := NewEncounters(fake.NewDB())
	n := 0
	r.newID = func() string { n++; return string(rune('a' + n - 1)) }
	r.now = func() time.Time { return clock }
	return r
}

func newEncounter(t *testing.T, r *Encounters, owner string) *Encounter {
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
	if _, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"position": 1}); err != nil {
		t.Fatal(err)
	}
	cur, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"position": 2})
	if !errors.Is(err, ErrRevisionConflict) || cur.Revision != 2 || cur.Position != 1 {
		t.Fatalf("err=%v revision=%d position=%d, want conflict at revision 2 position 1", err, cur.Revision, cur.Position)
	}
}

func TestEncounterUpdateRejectsCombatState(t *testing.T) {
	for _, f := range []string{"status", "round", "current_combatant_id", "turn_queue", "combatants"} {
		t.Run(f, func(t *testing.T) {
			r := newEncounters(t)
			e := newEncounter(t, r, "u1")
			_, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{f: "x"})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestEncounterCreateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Encounter)
	}{
		{"ordering", func(e *Encounter) { e.Ordering = "sideways" }},
		{"empty ordering", func(e *Encounter) { e.Ordering = "" }},
		{"tie breaker", func(e *Encounter) { e.TieBreaker = "coinflip" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newEncounters(t)
			e := &Encounter{Name: "x", Status: StatusDraft, Ordering: OrderHighToLow, TieBreaker: TieRandom}
			tt.mod(e)
			if err := r.Create(ctx, "u1", "u1", e); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateEncounter(t *testing.T) {
	base := func() *Encounter {
		return &Encounter{Status: StatusDraft, Ordering: OrderHighToLow, TieBreaker: TieRandom,
			Combatants: []Combatant{{ID: "c1"}, {ID: "c2"}}, TurnQueue: []string{"c1", "c2"}}
	}
	tests := []struct {
		name    string
		mod     func(*Encounter)
		invalid bool
	}{
		{"valid", func(*Encounter) {}, false},
		{"bad status", func(e *Encounter) { e.Status = "paused" }, true},
		{"queue entry not a combatant", func(e *Encounter) { e.TurnQueue = []string{"c1", "ghost"} }, true},
		{"duplicate combatant id", func(e *Encounter) { e.Combatants = []Combatant{{ID: "c1"}, {ID: "c1"}} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := base()
			tt.mod(e)
			if err := ValidateEncounter(e); errors.Is(err, ErrInvalid) != tt.invalid {
				t.Fatalf("err = %v, invalid want %v", err, tt.invalid)
			}
		})
	}
}

func TestEncounterCreateIgnoresCallerCombatState(t *testing.T) {
	r := newEncounters(t)
	e := &Encounter{Name: "x", Status: StatusActive, Round: 5, CurrentCombatantID: "c1",
		Ordering: OrderHighToLow, TieBreaker: TieRandom,
		Combatants: []Combatant{{ID: "c1"}}, TurnQueue: []string{"c1"}}
	if err := r.Create(ctx, "u1", "u1", e); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "u1", e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDraft || got.Round != 0 || got.CurrentCombatantID != "" || len(got.TurnQueue) != 0 {
		t.Fatalf("stored %q round %d current %q queue %v, want fresh draft", got.Status, got.Round, got.CurrentCombatantID, got.TurnQueue)
	}
}

func TestEncounterCreateRejectsDuplicateCombatantIDs(t *testing.T) {
	r := newEncounters(t)
	e := &Encounter{Name: "x", Ordering: OrderHighToLow, TieBreaker: TieRandom,
		Combatants: []Combatant{{ID: "c1"}, {ID: "c1"}}}
	if err := r.Create(ctx, "u1", "u1", e); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestEncounterUpdateRejectsBadEnumValues(t *testing.T) {
	tests := []struct {
		name string
		set  map[string]any
	}{
		{"ordering", map[string]any{"ordering": "sideways"}},
		{"ordering type", map[string]any{"ordering": 3}},
		{"tie breaker", map[string]any{"tie_breaker": "coinflip"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newEncounters(t)
			e := newEncounter(t, r, "u1")
			if _, err := r.Update(ctx, "u1", "u1", e.ID, 1, tt.set); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestEncounterSoftDeleteByOtherOwnerIsForbiddenAndKeepsIt(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if err := r.SoftDelete(ctx, "u2", "u2", e.ID, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if _, err := r.Get(ctx, "u1", e.ID); err != nil {
		t.Fatalf("encounter gone after other owner's delete: %v", err)
	}
}

func TestEncounterSoftDeleteWithStaleRevisionConflicts(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if _, err := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"name": "Renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SoftDelete(ctx, "u1", "u1", e.ID, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
}

func TestEncounterUpdateOnSoftDeletedIsNotFound(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if err := r.SoftDelete(ctx, "u1", "u1", e.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Update(ctx, "u1", "u1", e.ID, 2, map[string]any{"name": "Back"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
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
