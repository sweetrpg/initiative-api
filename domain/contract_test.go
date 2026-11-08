package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// updateAllowed reports whether fixtures may be rewritten: only with UPDATE_FIXTURES=1 and never
// in CI (the CI variable is set by GitHub Actions), where drift must fail, not heal itself.
func updateAllowed(env func(string) string) (bool, error) {
	if env("UPDATE_FIXTURES") != "1" {
		return false, nil
	}
	if env("CI") != "" {
		return false, errors.New("UPDATE_FIXTURES is set in CI; fixtures are never rewritten there")
	}
	return true, nil
}

// diffFixture returns an error when got differs from the fixture file name in dir.
func diffFixture(dir, name string, got []byte) error {
	want, err := os.ReadFile(filepath.Join(dir, name+".json")) //nolint:gosec // fixed test fixture directory
	if err != nil {
		return fmt.Errorf("missing fixture %s (run with UPDATE_FIXTURES=1 locally): %w", name, err)
	}
	if string(got) != string(want) {
		return fmt.Errorf("%s differs from its fixture:\n got: %s\nwant: %s", name, got, want)
	}
	return nil
}

func compare(t *testing.T, name string, got []byte) {
	t.Helper()
	update, err := updateAllowed(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if update {
		if err := os.MkdirAll(fixtureDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, name+".json"), got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := diffFixture(fixtureDir, name, got); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureComparisonFailsOnDrift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{\"a\": 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := diffFixture(dir, "x", []byte("{\"a\": 1}\n")); err != nil {
		t.Fatalf("identical output reported as drift: %v", err)
	}
	if err := diffFixture(dir, "x", []byte("{\"a\": 2}\n")); err == nil {
		t.Fatal("changed output was not reported as drift")
	}
	if err := diffFixture(dir, "absent", []byte("{}")); err == nil {
		t.Fatal("a missing fixture was not reported")
	}
}

func TestFixturesAreNeverRewrittenInCI(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		allowed bool
		fails   bool
	}{
		{"no flags", map[string]string{}, false, false},
		{"local update", map[string]string{"UPDATE_FIXTURES": "1"}, true, false},
		{"update in CI", map[string]string{"UPDATE_FIXTURES": "1", "CI": "true"}, false, true},
		{"CI without update", map[string]string{"CI": "true"}, false, false},
		{"other value", map[string]string{"UPDATE_FIXTURES": "yes"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := updateAllowed(func(k string) string { return tt.env[k] })
			if ok != tt.allowed || (err != nil) != tt.fails {
				t.Fatalf("allowed=%v err=%v, want allowed=%v fails=%v", ok, err, tt.allowed, tt.fails)
			}
		})
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
	expect(t, "created", e, data.StatusDraft, 1, 0, nil)

	got, err := s.Get(ctx, "user-1", e.ID)
	if err != nil || got.ID != e.ID || got.Revision != e.Revision {
		t.Fatalf("fetch after create: %v, id/revision must match the created encounter", err)
	}
	e, err = s.Start(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_started", e)
	expect(t, "started", e, data.StatusActive, 2, 1, []string{"c1", "c2"})

	e, err = s.Advance(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_advanced", e)
	expect(t, "advanced", e, data.StatusActive, 3, 1, []string{"c1", "c2"})
	if e.CurrentCombatantID != "c2" {
		t.Fatalf("advanced: current = %q, want c2", e.CurrentCombatantID)
	}

	e, err = s.Reset(ctx, "user-1", "user-1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "encounter_reset", e)
	expect(t, "reset", e, data.StatusDraft, 4, 0, nil)
}

// expect asserts the fields clients branch on, so a wrong value cannot hide behind a regenerated
// fixture.
func expect(t *testing.T, step string, e data.Encounter, status string, revision int64, round int, queue []string) {
	t.Helper()
	if e.Status != status || e.Revision != revision || e.Round != round || !slices.Equal(e.TurnQueue, queue) {
		t.Fatalf("%s: status=%q revision=%d round=%d queue=%v, want %q %d %d %v",
			step, e.Status, e.Revision, e.Round, e.TurnQueue, status, revision, round, queue)
	}
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

// errorClassesNote explains the read/write split in the access classes.
const errorClassesNote = "Reading another user's record is not_found, so a GET never reveals that the ID exists; " +
	"writing to it is forbidden (docs/coding-conventions.md, Owner-Scoped Data Access), classified only after the owner-scoped write has matched nothing."

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
	checkFixed(t, "error_classes", struct {
		Note  string      `json:"note"`
		Cases []errorCase `json:"cases"`
	}{errorClassesNote, cases})
}
