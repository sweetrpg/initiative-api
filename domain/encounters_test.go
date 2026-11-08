package domain

import (
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func newEncService(t *testing.T) *Encounters {
	t.Helper()
	db := fake.NewDB()
	s := NewEncounters(db)
	n := 0
	s.newID = func() string { n++; return "c" + string(rune('0'+n)) }
	hp := 0
	sys := data.GameSystem{ID: "test", Name: "Test", Family: "f", Aliases: []string{"test"},
		Fields: []data.TrackerField{{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 10, Min: &hp}}}
	if _, err := data.NewSystemRepo(db).Upsert(ctx, sys); err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnc(t *testing.T, s *Encounters, owner string, cs ...data.Combatant) data.Encounter {
	t.Helper()
	e, err := s.Create(ctx, owner, owner, data.Encounter{Name: " Ambush ", GameSystemID: "test",
		Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom, Combatants: cs})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func orc() data.Combatant {
	return data.Combatant{Name: "Orc", Kind: data.KindAdversary, Health: map[string]int{"hp": 3},
		Conditions: []data.Condition{{Name: "Stunned"}}}
}

func TestEncounterCreateValidates(t *testing.T) {
	tests := []struct {
		name string
		enc  data.Encounter
	}{
		{"no name", data.Encounter{Name: " ", GameSystemID: "test", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}},
		{"unknown system", data.Encounter{Name: "x", GameSystemID: "nope", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}},
		{"bad ordering", data.Encounter{Name: "x", GameSystemID: "test", Ordering: "z", TieBreaker: data.TieRandom}},
		{"bad combatant kind", data.Encounter{Name: "x", GameSystemID: "test", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom,
			Combatants: []data.Combatant{{Name: "a", Kind: "dragon"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newEncService(t).Create(ctx, "u1", "u1", tt.enc); !errors.Is(err, data.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestEncounterCreateTrimsNameAndAssignsCombatantIDs(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	if e.Name != "Ambush" || e.Combatants[0].ID != "c1" || e.Status != data.StatusDraft {
		t.Fatalf("encounter = %+v", e)
	}
}

func TestCombatantAddUpdateRemoveRestore(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	e, err := s.AddCombatant(ctx, "u1", "u1", e.ID, e.Revision, orc())
	if err != nil || len(e.Combatants) != 1 || e.Revision != 2 {
		t.Fatalf("add: err=%v enc=%+v", err, e)
	}
	id := e.Combatants[0].ID
	name, init := "Chief", 18.0
	e, err = s.UpdateCombatant(ctx, "u1", "u1", e.ID, e.Revision, id, CombatantPatch{Name: &name, Initiative: &init, Health: map[string]int{"hp": 9}})
	c := e.Combatants[0]
	if err != nil || c.Name != "Chief" || *c.Initiative != 18 || c.Health["hp"] != 9 || c.Kind != data.KindAdversary {
		t.Fatalf("update: err=%v combatant=%+v", err, c)
	}
	e, err = s.RemoveCombatant(ctx, "u1", "u1", e.ID, e.Revision, id)
	if err != nil || !e.Combatants[0].Removed {
		t.Fatalf("remove: err=%v enc=%+v", err, e)
	}
	e, err = s.RestoreCombatant(ctx, "u1", "u1", e.ID, e.Revision, id)
	if err != nil || e.Combatants[0].Removed || e.Revision != 5 {
		t.Fatalf("restore: err=%v enc=%+v", err, e)
	}
}

func TestRemoveCombatantLeavesQueueAndCurrent(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc(), orc())
	e, err := s.repo.SetState(ctx, "u1", "u1", e.ID, e.Revision, data.EncounterState{
		Status: data.StatusActive, Round: 1, CurrentCombatantID: "c1", TurnQueue: []string{"c1", "c2"}, Combatants: e.Combatants})
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.RemoveCombatant(ctx, "u1", "u1", e.ID, e.Revision, "c1")
	if err != nil || len(e.TurnQueue) != 1 || e.TurnQueue[0] != "c2" || e.CurrentCombatantID != "" {
		t.Fatalf("err=%v queue=%v current=%q", err, e.TurnQueue, e.CurrentCombatantID)
	}
}

func TestCombatantEditsRejectBadInput(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	empty, bad := " ", "dragon"
	tests := []struct {
		name string
		call func() error
	}{
		{"add no name", func() error {
			_, err := s.AddCombatant(ctx, "u1", "u1", e.ID, 1, data.Combatant{Kind: data.KindPC})
			return err
		}},
		{"patch empty name", func() error {
			_, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, 1, "c1", CombatantPatch{Name: &empty})
			return err
		}},
		{"patch bad kind", func() error {
			_, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, 1, "c1", CombatantPatch{Kind: &bad})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, data.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestMissingCombatantIsNotFound(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	if _, err := s.RemoveCombatant(ctx, "u1", "u1", e.ID, 1, "nope"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOtherOwnersEncounterWritesAreForbidden(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	name := "x"
	tests := []struct {
		name string
		call func() error
	}{
		{"settings", func() error {
			_, err := s.UpdateSettings(ctx, "u2", "u2", e.ID, 1, map[string]any{"name": "x"})
			return err
		}},
		{"delete", func() error { return s.Delete(ctx, "u2", "u2", e.ID, 1) }},
		{"add combatant", func() error { _, err := s.AddCombatant(ctx, "u2", "u2", e.ID, 1, orc()); return err }},
		{"update combatant", func() error {
			_, err := s.UpdateCombatant(ctx, "u2", "u2", e.ID, 1, "c1", CombatantPatch{Name: &name})
			return err
		}},
		{"start", func() error { _, err := s.Start(ctx, "u2", "u2", e.ID, 1); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, data.ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestStaleRevisionConflicts(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	if _, err := s.AddCombatant(ctx, "u1", "u1", e.ID, 1, orc()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCombatant(ctx, "u1", "u1", e.ID, 1, orc()); !errors.Is(err, data.ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	if _, err := s.End(ctx, "u1", "u1", e.ID, e.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("end from draft err = %v, want ErrTransition", err)
	}
	e, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || e.Status != data.StatusActive || e.Round != 1 {
		t.Fatalf("start: err=%v status=%s round=%d", err, e.Status, e.Round)
	}
	if _, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("start from active err = %v, want ErrTransition", err)
	}
	e, err = s.End(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || e.Status != data.StatusEnded {
		t.Fatalf("end: err=%v status=%s", err, e.Status)
	}
	if _, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("start from ended err = %v, want ErrTransition", err)
	}
}

func TestResetRestoresDraftDefaultsAndClearsConditions(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	e, _ = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	e, err := s.Reset(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	c := e.Combatants[0]
	if e.Status != data.StatusDraft || e.Round != 0 || e.CurrentCombatantID != "" || c.Health["hp"] != 10 || len(c.Conditions) != 0 {
		t.Fatalf("after reset: status=%s round=%d health=%v conditions=%v", e.Status, e.Round, c.Health, c.Conditions)
	}
}

func TestResetOfOthersEncounterIsForbiddenAndAbsentIsNotFound(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	if _, err := s.Reset(ctx, "u2", "u2", e.ID, 1); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("other owner err = %v, want ErrForbidden", err)
	}
	if _, err := s.Reset(ctx, "u1", "u1", "nope", 1); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("absent err = %v, want ErrNotFound", err)
	}
}

func TestResetClearsStaleTurnQueue(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	e, err := s.repo.SetState(ctx, "u1", "u1", e.ID, e.Revision, data.EncounterState{
		Status: data.StatusActive, Round: 2, CurrentCombatantID: "c1", TurnQueue: []string{"c1"}, Combatants: e.Combatants})
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.Reset(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || len(e.TurnQueue) != 0 {
		t.Fatalf("err=%v queue=%v, want empty", err, e.TurnQueue)
	}
}

func TestEndFromEndedIsRejected(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	e, _ = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	e, _ = s.End(ctx, "u1", "u1", e.ID, e.Revision)
	if _, err := s.End(ctx, "u1", "u1", e.ID, e.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("err = %v, want ErrTransition", err)
	}
}

func TestHealthIsValidatedAgainstTheDefinition(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1", orc())
	low, ok := -5, map[string]int{"hp": 4}
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"add below min", func() error {
			c := orc()
			c.Health = map[string]int{"hp": low}
			_, err := s.AddCombatant(ctx, "u1", "u1", e.ID, 1, c)
			return err
		}, "hp"},
		{"add unknown key", func() error {
			c := orc()
			c.Health = map[string]int{"mana": 1}
			_, err := s.AddCombatant(ctx, "u1", "u1", e.ID, 1, c)
			return err
		}, "mana"},
		{"patch below min", func() error {
			_, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, 1, "c1", CombatantPatch{Health: map[string]int{"hp": low}})
			return err
		}, "hp"},
		{"create below min", func() error {
			c := orc()
			c.Health = map[string]int{"hp": low}
			_, err := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: "test",
				Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom, Combatants: []data.Combatant{c}})
			return err
		}, "hp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var he *HealthError
			if !errors.Is(err, data.ErrInvalid) || !errors.As(err, &he) || he.Field != tt.want {
				t.Fatalf("err = %v, want HealthError on %q", err, tt.want)
			}
		})
	}
	if _, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, 1, "c1", CombatantPatch{Health: ok}); err != nil {
		t.Fatalf("valid health rejected: %v", err)
	}
}

func TestCheckHealthBounds(t *testing.T) {
	zero, max := 0, 20
	sys := data.GameSystem{Fields: []data.TrackerField{
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

func TestSettingsCannotSetCombatState(t *testing.T) {
	s := newEncService(t)
	e := newEnc(t, s, "u1")
	if _, err := s.UpdateSettings(ctx, "u1", "u1", e.ID, 1, map[string]any{"status": data.StatusActive}); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
