package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// encounterOp is one encounter write, with the state it needs before it can succeed.
type encounterOp struct {
	name string
	prep func(t *testing.T, s *Encounters, e data.Encounter) data.Encounter
	call func(s *Encounters, owner string, e data.Encounter, rev int64) error
}

func noPrep(_ *testing.T, _ *Encounters, e data.Encounter) data.Encounter { return e }

func prepStart(t *testing.T, s *Encounters, e data.Encounter) data.Encounter {
	t.Helper()
	e, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func prepAdvance(t *testing.T, s *Encounters, e data.Encounter) data.Encounter {
	t.Helper()
	e = prepStart(t, s, e)
	e, err := s.Advance(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func encounterOps() []encounterOp {
	one := func(err error) error { return err }
	name := "Renamed"
	return []encounterOp{
		{"update settings", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.UpdateSettings(ctx, o, o, e.ID, r, map[string]any{"name": "x"})
			return one(err)
		}},
		{"update ordering", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.UpdateSettings(ctx, o, o, e.ID, r, map[string]any{"ordering": data.OrderLowToHigh})
			return err
		}},
		{"add combatant", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.AddCombatant(ctx, o, o, e.ID, r, scored("New", data.KindPC, 1))
			return err
		}},
		{"update combatant", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.UpdateCombatant(ctx, o, o, e.ID, r, e.Combatants[0].ID, CombatantPatch{Name: &name})
			return err
		}},
		{"remove combatant", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.RemoveCombatant(ctx, o, o, e.ID, r, e.Combatants[0].ID)
			return err
		}},
		{"restore combatant", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.RestoreCombatant(ctx, o, o, e.ID, r, e.Combatants[0].ID)
			return err
		}},
		{"start", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.Start(ctx, o, o, e.ID, r)
			return err
		}},
		{"end", prepStart, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.End(ctx, o, o, e.ID, r)
			return err
		}},
		{"advance", prepStart, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.Advance(ctx, o, o, e.ID, r)
			return err
		}},
		{"step back", prepAdvance, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.StepBack(ctx, o, o, e.ID, r)
			return err
		}},
		{"reset", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			_, err := s.Reset(ctx, o, o, e.ID, r)
			return err
		}},
		{"delete", noPrep, func(s *Encounters, o string, e data.Encounter, r int64) error {
			return s.Delete(ctx, o, o, e.ID, r)
		}},
	}
}

func opEncounter(t *testing.T, op encounterOp) (*Encounters, data.Encounter) {
	t.Helper()
	s := newTurnService(t)
	e := newEnc(t, s, "u1", scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	return s, op.prep(t, s, e)
}

// snapshot reads the whole stored encounter, for before/after comparison.
func snapshot(t *testing.T, s *Encounters, id string) data.Encounter {
	t.Helper()
	e, err := s.Get(ctx, "u1", id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// requireUnchanged fails unless the stored encounter is identical, field for field, to before.
func requireUnchanged(t *testing.T, s *Encounters, before data.Encounter) {
	t.Helper()
	after := snapshot(t, s, before.ID)
	if !reflect.DeepEqual(before, after) {
		b, _ := json.MarshalIndent(before, "", " ")
		a, _ := json.MarshalIndent(after, "", " ")
		t.Fatalf("rejected call changed the encounter\nbefore: %s\nafter:  %s", b, a)
	}
}

func TestEveryEncounterWriteChecksTheRevision(t *testing.T) {
	for _, op := range encounterOps() {
		t.Run(op.name, func(t *testing.T) {
			s, e := opEncounter(t, op)
			before := snapshot(t, s, e.ID)
			if err := op.call(s, "u1", e, e.Revision+3); !errors.Is(err, data.ErrRevisionConflict) {
				t.Fatalf("stale revision err = %v, want ErrRevisionConflict", err)
			}
			requireUnchanged(t, s, before)
			if err := op.call(s, "u1", e, e.Revision); err != nil {
				t.Fatalf("current revision err = %v, want success", err)
			}
		})
	}
}

func TestEveryEncounterWriteIsOwnerChecked(t *testing.T) {
	for _, op := range encounterOps() {
		t.Run(op.name, func(t *testing.T) {
			s, e := opEncounter(t, op)
			before := snapshot(t, s, e.ID)
			if err := op.call(s, "u2", e, e.Revision); !errors.Is(err, data.ErrForbidden) {
				t.Fatalf("other owner err = %v, want ErrForbidden", err)
			}
			requireUnchanged(t, s, before)
			missing := e
			missing.ID = "absent"
			if err := op.call(s, "u1", missing, e.Revision); !errors.Is(err, data.ErrNotFound) {
				t.Fatalf("absent encounter err = %v, want ErrNotFound", err)
			}
			requireUnchanged(t, s, before)
		})
	}
}

func TestEncounterListIsOwnerScopedFilteredAndPaged(t *testing.T) {
	s := newTurnService(t)
	for i := range 3 {
		e := newEnc(t, s, "u1")
		if i == 0 {
			if _, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	newEnc(t, s, "u2")

	all, err := s.List(ctx, "u1", data.Query{})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("u1 list: %d items, err %v, want 3 (u2's must not appear)", len(all.Items), err)
	}
	active, err := s.List(ctx, "u1", data.Query{Filters: []data.Filter{{Field: "status", Op: data.OpEq, Values: []any{data.StatusActive}}}})
	if err != nil || len(active.Items) != 1 {
		t.Fatalf("status filter: %d items, err %v, want 1", len(active.Items), err)
	}
	first, err := s.List(ctx, "u1", data.Query{Limit: 2, Sort: []data.SortKey{{Field: "created_at"}}})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("page 1: %d items cursor %q err %v", len(first.Items), first.NextCursor, err)
	}
	second, err := s.List(ctx, "u1", data.Query{Limit: 2, Sort: []data.SortKey{{Field: "created_at"}}, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("page 2: %d items cursor %q err %v", len(second.Items), second.NextCursor, err)
	}
	if _, err := s.List(ctx, "u1", data.Query{Filters: []data.Filter{{Field: "owner_user_id", Op: data.OpEq, Values: []any{"u2"}}}}); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("filtering by owner err = %v, want ErrInvalid", err)
	}
}

func TestRosterListIsOwnerScoped(t *testing.T) {
	s := newService(t)
	mustRoster(t, s, "u1")
	mustRoster(t, s, "u1")
	mustRoster(t, s, "u2")
	page, err := s.List(ctx, "u1", data.Query{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("u1 list: %d items, err %v, want 2", len(page.Items), err)
	}
	if _, err := s.List(ctx, "", data.Query{}); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("empty owner err = %v, want ErrInvalid", err)
	}
}

func TestRosterRenameValidatesAndChecksRevision(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	if _, err := s.Rename(ctx, "u1", "u1", r.ID, 1, " ", false); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("blank name err = %v, want ErrInvalid", err)
	}
	if _, err := s.Rename(ctx, "u1", "u1", r.ID, 9, "x", false); !errors.Is(err, data.ErrRevisionConflict) {
		t.Fatalf("stale err = %v, want ErrRevisionConflict", err)
	}
}

// Each ordering, end to end through Start: the queue the service persists.
func TestOrderingModesThroughTheService(t *testing.T) {
	mixed := func() []data.Combatant {
		return []data.Combatant{
			scored("Goblin", data.KindAdversary, 20), scored("Pia", data.KindPC, 3),
			scored("Pat", data.KindPC, 9), scored("Door", data.KindObject, 9),
		}
	}
	tests := []struct {
		name     string
		ordering string
		tie      string
		want     []string
	}{
		{"high to low, list-order ties", data.OrderHighToLow, data.TieQuery, []string{"Goblin", "Pat", "Door", "Pia"}},
		{"high to low, alphabetical ties", data.OrderHighToLow, data.TieAlphabetical, []string{"Goblin", "Door", "Pat", "Pia"}},
		{"low to high", data.OrderLowToHigh, data.TieQuery, []string{"Pia", "Pat", "Door", "Goblin"}},
		{"pc vs adversary", data.OrderPCVsAdversary, data.TieQuery, []string{"Pat", "Pia", "Goblin", "Door"}},
		{"player managed keeps list order", data.OrderPlayerManaged, data.TieQuery, []string{"Goblin", "Pia", "Pat", "Door"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTurnService(t)
			e, err := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: "test",
				Ordering: tt.ordering, TieBreaker: tt.tie, Combatants: mixed()})
			if err != nil {
				t.Fatal(err)
			}
			e, err = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
			if err != nil || !slices.Equal(names(e, e.TurnQueue), tt.want) {
				t.Fatalf("err=%v queue=%v, want %v", err, names(e, e.TurnQueue), tt.want)
			}
		})
	}
}

func TestProductionIDAndShuffleSourcesAreUsable(t *testing.T) {
	db := fake.NewDB()
	e, r := NewEncounters(db), NewRosters(db)
	if a, b := e.newID(), e.newID(); a == "" || a == b {
		t.Fatalf("encounter IDs %q, %q, want distinct and non-empty", a, b)
	}
	if a, b := r.newID(), r.newID(); a == "" || a == b {
		t.Fatalf("roster IDs %q, %q, want distinct and non-empty", a, b)
	}
	p := e.perm(5)
	slices.Sort(p)
	if !slices.Equal(p, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("perm(5) sorted = %v, want 0..4", p)
	}
}
