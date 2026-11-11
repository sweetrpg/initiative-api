package data

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func newEncounters(t *testing.T) *Encounters {
	t.Helper()
	r := NewEncounters(fake.NewDB())
	n := 0
	r.newID = func() string { n++; return testID(string(rune('a' + n - 1))) }
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
			Combatants: []Combatant{cb("c1"), cb("c2")}, TurnQueue: []string{"c1", "c2"}}
	}
	tests := []struct {
		name    string
		mod     func(*Encounter)
		invalid bool
	}{
		{"valid", func(*Encounter) {}, false},
		{"bad status", func(e *Encounter) { e.Status = "paused" }, true},
		{"queue entry not a combatant", func(e *Encounter) { e.TurnQueue = []string{"c1", "ghost"} }, true},
		{"duplicate combatant id", func(e *Encounter) { e.Combatants = []Combatant{cb("c1"), cb("c1")} }, true},
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
		Combatants: []Combatant{cb("c1")}, TurnQueue: []string{"c1"}}
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
		Combatants: []Combatant{cb("c1"), cb("c1")}}
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

func TestStateWriteOnlyWritesOnlyStateFields(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if _, err := r.updateState(ctx, "u1", "u1", e.ID, 1, map[string]any{"name": "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestSetStateValidates(t *testing.T) {
	tests := []struct {
		name string
		st   EncounterState
	}{
		{"bad status", EncounterState{Status: "paused"}},
		{"negative round", EncounterState{Status: StatusActive, Round: -1}},
		{"current not a combatant", EncounterState{Status: StatusActive, CurrentCombatantID: "ghost"}},
		{"queue not a combatant", EncounterState{Status: StatusActive, TurnQueue: []string{"ghost"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newEncounters(t)
			e := newEncounter(t, r, "u1")
			if _, err := r.SetState(ctx, "u1", "u1", e.ID, 1, tt.st); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSetStateAdvancesRevisionOnce(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	got, err := r.SetState(ctx, "u1", "u1", e.ID, 1, EncounterState{Status: StatusActive, Round: 1,
		CurrentCombatantID: "c1", TurnQueue: []string{"c1"}, Combatants: []Combatant{cb("c1")}})
	if err != nil || got.Revision != 2 || got.Status != StatusActive || got.CurrentCombatantID != "c1" {
		t.Fatalf("err=%v got=%+v", err, got)
	}
}

func TestSetStateRejectsDuplicateCombatantID(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	_, err := r.SetState(ctx, "u1", "u1", e.ID, 1, EncounterState{Status: StatusActive,
		Combatants: []Combatant{cb("c1"), cb("c1")}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func cb(id string) Combatant { return Combatant{ID: id, Name: "n" + id, Kind: KindPC} }

func TestValidateStateCombatantRules(t *testing.T) {
	one := 1
	zero := 0
	tests := []struct {
		name string
		c    Combatant
	}{
		{"no name", Combatant{ID: "c1", Name: " ", Kind: KindPC}},
		{"bad kind", Combatant{ID: "c1", Name: "a", Kind: "dragon"}},
		{"unnamed condition", Combatant{ID: "c1", Name: "a", Kind: KindPC, Conditions: []Condition{{Name: " "}}}},
		{"zero rounds condition", Combatant{ID: "c1", Name: "a", Kind: KindPC, Conditions: []Condition{{Name: "x", RoundsRemaining: &zero}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateState(EncounterState{Status: StatusDraft, Combatants: []Combatant{tt.c}}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
	ok := Combatant{ID: "c1", Name: "a", Kind: KindPC, Conditions: []Condition{{Name: "x", RoundsRemaining: &one}}}
	if err := ValidateState(EncounterState{Status: StatusDraft, Combatants: []Combatant{ok}}); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
}

func TestSetStateChecksHealthAgainstTheDefinition(t *testing.T) {
	db := fake.NewDB()
	zero := 0
	sys := GameSystem{ID: "test", Name: "T", Family: "f", Aliases: []string{"test"},
		Fields: []TrackerField{{Key: "hp", Label: "HP", Kind: FieldCounter, Min: &zero}}}
	if _, err := NewSystemRepo(db).Upsert(ctx, sys); err != nil {
		t.Fatal(err)
	}
	r := NewEncounters(db)
	e := &Encounter{Name: "x", GameSystemID: "test", Ordering: OrderHighToLow, TieBreaker: TieRandom}
	if err := r.Create(ctx, "u1", "u1", e); err != nil {
		t.Fatal(err)
	}
	bad := cb("c1")
	bad.Health = map[string]int{"hp": -5}
	_, err := r.SetState(ctx, "u1", "u1", e.ID, 1, EncounterState{Status: StatusDraft, Combatants: []Combatant{bad}})
	var he *HealthError
	if !errors.As(err, &he) || he.Field != "hp" {
		t.Fatalf("err = %v, want HealthError on hp", err)
	}
}

func TestCheckHealthBounds(t *testing.T) {
	zero, max := 0, 20
	sys := GameSystem{Fields: []TrackerField{
		{Key: "hp", Min: &zero, MaxRef: "max_hp"},
		{Key: "max_hp", Min: &zero, Max: &max},
	}}
	tests := []struct {
		name string
		h    map[string]int
		bad  bool
	}{
		{"ok", map[string]int{"hp": 5, "max_hp": 10}, false},
		{"at min", map[string]int{"hp": 0}, false},
		{"above max", map[string]int{"max_hp": 21}, true},
		{"above max_ref", map[string]int{"hp": 11, "max_hp": 10}, true},
		{"max_ref absent", map[string]int{"hp": 50}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckHealth(sys, tt.h); (err != nil) != tt.bad {
				t.Fatalf("err = %v, bad want %v", err, tt.bad)
			}
		})
	}
}

func TestWriteAppliesSettingsAndStateInOneRevision(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	got, err := r.Write(ctx, "u1", "u1", e.ID, 1, map[string]any{"ordering": OrderLowToHigh},
		EncounterState{Status: StatusDraft, TurnQueue: []string{"c1"}, Combatants: []Combatant{cb("c1")}})
	if err != nil || got.Revision != 2 || got.Ordering != OrderLowToHigh || got.TurnQueue[0] != "c1" {
		t.Fatalf("err=%v got=%+v, want one write at revision 2", err, got)
	}
}

func TestWriteRejectsBadSettingsAndStaleRevision(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	st := EncounterState{Status: StatusDraft}
	if _, err := r.Write(ctx, "u1", "u1", e.ID, 1, map[string]any{"ordering": "z"}, st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad ordering err = %v, want ErrInvalid", err)
	}
	if _, err := r.Write(ctx, "u1", "u1", e.ID, 1, map[string]any{"status": "active"}, st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state key as setting err = %v, want ErrInvalid", err)
	}
	if _, err := r.Write(ctx, "u1", "u1", e.ID, 9, nil, st); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale err = %v, want ErrRevisionConflict", err)
	}
}

func TestUpdateStateRejectsEmptyAndUnknownFields(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	for name, set := range map[string]map[string]any{"empty": {}, "settings field": {"name": "x"}} {
		if _, err := r.updateState(ctx, "u1", "u1", e.ID, 1, set); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := r.updateState(ctx, "", "", e.ID, 1, map[string]any{"round": 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty owner err = %v, want ErrInvalid", err)
	}
}

func TestMissingClassifiesButNeverGrantsAccess(t *testing.T) {
	r := newEncounters(t)
	e := newEncounter(t, r, "u1")
	if err := r.Missing(ctx, "u1", e.ID); err != nil {
		t.Fatalf("visible record: err = %v, want nil", err)
	}
	if err := r.Missing(ctx, "u2", e.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other owner err = %v, want ErrForbidden", err)
	}
	if err := r.Missing(ctx, "u1", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent err = %v, want ErrNotFound", err)
	}
}

func TestListAndCountRequireAnOwner(t *testing.T) {
	r := newEncounters(t)
	if _, err := r.List(ctx, "", Query{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list err = %v, want ErrInvalid", err)
	}
	if _, err := r.Count(ctx, "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("count err = %v, want ErrInvalid", err)
	}
	if _, err := r.Count(ctx, "u1", []Filter{{Field: "owner_user_id", Op: OpEq, Values: []any{"u2"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("count by owner err = %v, want ErrInvalid", err)
	}
}

func TestHealthErrorNamesTheFieldAndReason(t *testing.T) {
	msg := (&HealthError{Field: "hp", Reason: "too high"}).Error()
	if !strings.Contains(msg, "hp") || !strings.Contains(msg, "too high") {
		t.Fatalf("message %q should name the field and reason", msg)
	}
}

func TestFindByExternalRefRejectsBadInput(t *testing.T) {
	e := NewEncounters(fake.NewDB())
	long := strings.Repeat("x", maxRefLen+1)
	for _, tt := range []struct{ owner, key, val string }{{"", "k", "v"}, {"u1", "", "v"}, {"u1", "k", ""}, {"u1", long, "v"}, {"u1", "k", long}} {
		if _, err := e.FindByExternalRef(ctx, tt.owner, tt.key, tt.val); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v err = %v, want ErrInvalid", tt, err)
		}
	}
}
