package domain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func ip1() *int { one := 1; return &one }

// Contract tests pin the JSON shape of every resource and the error classes a client can see.
// Each scenario's normalized output is compared with a file in testdata/contract; clients (web,
// desktop, iOS, the Foundry module) load the same files as fixtures, so a change to the wire
// contract fails here first. Regenerate after an intentional change with UPDATE_FIXTURES=1.

const fixtureDir = "../testdata/contract"

const fixedTime = "2026-01-01T00:00:00Z"

// stableJSON replaces the values that differ between runs (generated ID, clock) so output is stable.
func stableJSON(t *testing.T, v any, generatedID bool) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if m, ok := doc.(map[string]any); ok {
		for _, k := range []string{"created_at", "updated_at"} {
			if _, ok := m[k]; ok {
				m[k] = fixedTime
			}
		}
		if _, ok := m["id"]; ok && generatedID {
			m["id"] = "GENERATED_ID"
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func checkFixture(t *testing.T, name string, v any) {
	t.Helper()
	compare(t, name, stableJSON(t, v, true))
}

func checkFixed(t *testing.T, name string, v any) {
	t.Helper()
	compare(t, name, stableJSON(t, v, false))
}

func compare(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(fixtureDir, name+".json")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(fixtureDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture directory
	if err != nil {
		t.Fatalf("missing fixture %s (run with UPDATE_FIXTURES=1): %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from its fixture:\n got: %s\nwant: %s", name, got, want)
	}
}

func contractEncounter(t *testing.T, s *Encounters) data.Encounter {
	t.Helper()
	a, b := orc(), orc()
	a.Name, a.Initiative, a.Conditions = "Orc Chief", f(18), nil
	two := 1
	b.Name, b.Initiative, b.Conditions = "Orc Scout", f(12), []data.Condition{{Name: "Stunned", RoundsRemaining: &two}}
	e, err := s.Create(ctx, "user-1", "user-1", data.Encounter{Name: "Ambush", GameSystemID: "test",
		Ordering: data.OrderHighToLow, TieBreaker: data.TieQuery, Combatants: []data.Combatant{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestContractEncounterLifecycleResponses(t *testing.T) {
	s := newTurnService(t)
	e := contractEncounter(t, s)
	checkFixture(t, "encounter_created", e)

	got, err := s.Get(ctx, "user-1", e.ID)
	if err != nil || got.ID != e.ID || got.Revision != e.Revision {
		t.Fatalf("fetch after create: %v, id/revision must match the created encounter", err)
	}
	e, err = s.Start(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_started", e)

	e, err = s.Advance(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_advanced", e)

	e, err = s.Reset(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_reset", e)
}

func TestContractStaleRevisionReturnsTheCurrentEncounter(t *testing.T) {
	s := newTurnService(t)
	e := contractEncounter(t, s)
	started, err := s.Start(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.repo.Update(ctx, "user-1", "user-1", e.ID, e.Revision, map[string]any{"name": "Stale"})
	if !errors.Is(err, data.ErrRevisionConflict) || cur.Revision != started.Revision {
		t.Fatalf("err=%v current revision=%d, want conflict carrying revision %d", err, cur.Revision, started.Revision)
	}
	checkFixture(t, "encounter_revision_conflict_current", cur)
}

func TestContractRosterAndGameSystemResponses(t *testing.T) {
	s := newService(t)
	r, err := s.Create(ctx, "user-1", "user-1", "Goblin Camp", true, []data.Character{goblin()})
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "roster_created", r)

	db := fake.NewDB()
	zero := 0
	repo := data.NewSystemRepo(db)
	if _, err := repo.Upsert(ctx, data.GameSystem{ID: "dnd5e", Name: "Dungeons & Dragons 5th Edition", Family: "dnd5",
		Aliases: []string{"dnd5"}, Gated: true, Fields: []data.TrackerField{
			{Key: "current", Label: "initiative.health.current", Kind: data.FieldCounter, Def: 1, Min: &zero, MaxRef: "maximum"},
			{Key: "maximum", Label: "initiative.health.maximum", Kind: data.FieldCounter, Def: 1, Min: ip1()},
		}}); err != nil {
		t.Fatal(err)
	}
	sys, err := repo.Get(ctx, "dnd5")
	if err != nil {
		t.Fatal(err)
	}
	checkFixed(t, "game_system", sys)
}

type errorCase struct {
	Scenario string `json:"scenario"`
	Class    string `json:"class"`
	Key      string `json:"key,omitempty"`
}

func classify(err error) (class, key string) {
	var he *data.HealthError
	switch {
	case err == nil:
		return "ok", ""
	case errors.As(err, &he):
		return "invalid_health", he.Field
	case errors.Is(err, ErrTransition):
		return "invalid_transition", ""
	case errors.Is(err, data.ErrNotFound):
		return "not_found", ""
	case errors.Is(err, data.ErrForbidden):
		return "forbidden", ""
	case errors.Is(err, data.ErrRevisionConflict):
		return "revision_conflict", ""
	case errors.Is(err, data.ErrInvalid):
		return "invalid", ""
	}
	return "internal", ""
}

func TestContractErrorClasses(t *testing.T) {
	s := newTurnService(t)
	e := contractEncounter(t, s)
	chief := e.Combatants[0].ID
	bad := func(h map[string]int) error {
		_, err := s.UpdateCombatant(ctx, "user-1", "user-1", e.ID, e.Revision, chief, CombatantPatch{Health: h})
		return err
	}
	scenarios := []struct {
		name string
		err  error
	}{
		{"read another user's encounter", func() error { _, err := s.Get(ctx, "user-2", e.ID); return err }()},
		{"read an encounter that does not exist", func() error { _, err := s.Get(ctx, "user-1", "absent"); return err }()},
		{"edit another user's encounter", func() error {
			_, err := s.UpdateSettings(ctx, "user-2", "user-2", e.ID, e.Revision, map[string]any{"name": "x"})
			return err
		}()},
		{"edit with a stale revision", func() error {
			_, err := s.UpdateSettings(ctx, "user-1", "user-1", e.ID, e.Revision+7, map[string]any{"name": "x"})
			return err
		}()},
		{"health above its maximum", bad(map[string]int{"hp": 50})},
		{"health below its minimum", bad(map[string]int{"hp": -5})},
		{"health key not in the definition", bad(map[string]int{"mana": 1})},
		{"unknown combatant", func() error {
			_, err := s.RemoveCombatant(ctx, "user-1", "user-1", e.ID, e.Revision, "absent")
			return err
		}()},
		{"advance a draft encounter", func() error { _, err := s.Advance(ctx, "user-1", "user-1", e.ID, e.Revision); return err }()},
		{"end a draft encounter", func() error { _, err := s.End(ctx, "user-1", "user-1", e.ID, e.Revision); return err }()},
		{"combat state through settings", func() error {
			_, err := s.UpdateSettings(ctx, "user-1", "user-1", e.ID, e.Revision, map[string]any{"status": "active"})
			return err
		}()},
		{"unknown game system", func() error {
			_, err := s.UpdateSettings(ctx, "user-1", "user-1", e.ID, e.Revision, map[string]any{"game_system_id": "absent"})
			return err
		}()},
	}
	cases := make([]errorCase, 0, len(scenarios))
	for _, sc := range scenarios {
		class, key := classify(sc.err)
		if class == "ok" || class == "internal" {
			t.Fatalf("%s: err = %v classified %q", sc.name, sc.err, class)
		}
		cases = append(cases, errorCase{sc.name, class, key})
	}
	checkFixed(t, "error_classes", cases)
}
