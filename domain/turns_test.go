package domain

import (
	"errors"
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
)

func newTurnService(t *testing.T) *Encounters {
	t.Helper()
	s := newEncService(t)
	s.perm = identity
	return s
}

func scored(name, kind string, init float64) data.Combatant {
	c := cbt("", name, kind, f(init))
	c.Health = nil
	return c
}

func startedEncounter(t *testing.T, s *Encounters, cs ...data.Combatant) data.Encounter {
	t.Helper()
	e := newEnc(t, s, "u1", cs...)
	e, err := s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func names(e data.Encounter, ids []string) []string {
	var out []string
	for _, id := range ids {
		for _, c := range e.Combatants {
			if c.ID == id {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

func TestStartBuildsQueueAndMakesFirstCombatantCurrent(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("Low", data.KindPC, 3), scored("High", data.KindPC, 20))
	if got := names(e, e.TurnQueue); !slices.Equal(got, []string{"High", "Low"}) {
		t.Fatalf("queue = %v", got)
	}
	if names(e, []string{e.CurrentCombatantID})[0] != "High" || e.Round != 1 {
		t.Fatalf("current = %q round = %d", e.CurrentCombatantID, e.Round)
	}
}

func TestStartTicksFirstCombatantsConditions(t *testing.T) {
	s := newTurnService(t)
	one := 1
	c := scored("High", data.KindPC, 20)
	c.Conditions = []data.Condition{{Name: "Stunned", RoundsRemaining: &one}}
	e := startedEncounter(t, s, c)
	if len(e.Combatants[0].Conditions) != 0 {
		t.Fatalf("conditions = %+v, want none", e.Combatants[0].Conditions)
	}
}

func TestStartWithNoCombatantsHasNoCurrent(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s)
	if e.CurrentCombatantID != "" || len(e.TurnQueue) != 0 {
		t.Fatalf("current=%q queue=%v", e.CurrentCombatantID, e.TurnQueue)
	}
}

func TestAdvanceWrapsAndIncrementsRound(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	var seq []string
	var rounds []int
	for range 4 {
		var err error
		e, err = s.Advance(ctx, "u1", "u1", e.ID, e.Revision)
		if err != nil {
			t.Fatal(err)
		}
		seq = append(seq, names(e, []string{e.CurrentCombatantID})[0])
		rounds = append(rounds, e.Round)
	}
	if !slices.Equal(seq, []string{"B", "A", "B", "A"}) || !slices.Equal(rounds, []int{1, 2, 2, 3}) {
		t.Fatalf("sequence %v rounds %v", seq, rounds)
	}
}

func TestAdvanceExpiresConditionOfCombatantWhoseTurnBegins(t *testing.T) {
	s := newTurnService(t)
	one := 1
	b := scored("B", data.KindPC, 10)
	b.Conditions = []data.Condition{{Name: "Stunned", RoundsRemaining: &one}}
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), b)
	if len(findByName(e, "B").Conditions) != 1 {
		t.Fatal("B's condition ticked before its turn")
	}
	e, err := s.Advance(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || len(findByName(e, "B").Conditions) != 0 {
		t.Fatalf("err=%v conditions=%+v, want Stunned removed when B's turn begins", err, findByName(e, "B").Conditions)
	}
}

func findByName(e data.Encounter, name string) data.Combatant {
	for _, c := range e.Combatants {
		if c.Name == name {
			return c
		}
	}
	return data.Combatant{}
}

func TestAdvanceRejectsNonActiveAndEmpty(t *testing.T) {
	s := newTurnService(t)
	draft := newEnc(t, s, "u1", scored("A", data.KindPC, 1))
	if _, err := s.Advance(ctx, "u1", "u1", draft.ID, draft.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("draft err = %v, want ErrTransition", err)
	}
	empty := startedEncounter(t, s)
	if _, err := s.Advance(ctx, "u1", "u1", empty.ID, empty.Revision); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("empty err = %v, want ErrInvalid", err)
	}
}

func TestStepBack(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	if _, err := s.StepBack(ctx, "u1", "u1", e.ID, e.Revision); !errors.Is(err, ErrTransition) {
		t.Fatalf("first turn of round 1 err = %v, want ErrTransition", err)
	}
	e, _ = s.Advance(ctx, "u1", "u1", e.ID, e.Revision) // B
	e, _ = s.Advance(ctx, "u1", "u1", e.ID, e.Revision) // A, round 2
	e, err := s.StepBack(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || findByID(e, e.CurrentCombatantID).Name != "B" || e.Round != 1 {
		t.Fatalf("err=%v current=%q round=%d, want B in round 1", err, e.CurrentCombatantID, e.Round)
	}
}

func findByID(e data.Encounter, id string) data.Combatant {
	for _, c := range e.Combatants {
		if c.ID == id {
			return c
		}
	}
	return data.Combatant{}
}

func TestInitiativeChangeReordersQueueAndKeepsCurrent(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	cur := e.CurrentCombatantID
	b := findByName(e, "B")
	e, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, e.Revision, b.ID, CombatantPatch{Initiative: f(30)})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(e, e.TurnQueue); !slices.Equal(got, []string{"B", "A"}) || e.CurrentCombatantID != cur {
		t.Fatalf("queue=%v current=%q want B,A with current unchanged", got, e.CurrentCombatantID)
	}
}

func TestAddAndRestoreRequeueCombatants(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20))
	e, err := s.AddCombatant(ctx, "u1", "u1", e.ID, e.Revision, scored("Z", data.KindPC, 25))
	if err != nil || names(e, e.TurnQueue)[0] != "Z" {
		t.Fatalf("add: err=%v queue=%v", err, names(e, e.TurnQueue))
	}
	z := findByName(e, "Z")
	e, _ = s.RemoveCombatant(ctx, "u1", "u1", e.ID, e.Revision, z.ID)
	if slices.Contains(e.TurnQueue, z.ID) {
		t.Fatal("removed combatant still queued")
	}
	e, _ = s.RestoreCombatant(ctx, "u1", "u1", e.ID, e.Revision, z.ID)
	if !slices.Contains(e.TurnQueue, z.ID) {
		t.Fatal("restored combatant not requeued")
	}
}

func TestRemovingCurrentPassesTurnToNextWrappingAtEnd(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	e, _ = s.Advance(ctx, "u1", "u1", e.ID, e.Revision) // B is last
	b := findByName(e, "B")
	e, err := s.RemoveCombatant(ctx, "u1", "u1", e.ID, e.Revision, b.ID)
	if err != nil || findByID(e, e.CurrentCombatantID).Name != "A" || e.Round != 2 {
		t.Fatalf("err=%v current=%q round=%d, want A in round 2", err, e.CurrentCombatantID, e.Round)
	}
}

func TestChangingOrderingRecomputesQueueInOneWrite(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	e, err := s.UpdateSettings(ctx, "u1", "u1", e.ID, e.Revision, map[string]any{"ordering": data.OrderLowToHigh})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(e, e.TurnQueue); !slices.Equal(got, []string{"B", "A"}) || e.Revision != 3 {
		t.Fatalf("queue=%v revision=%d, want B,A at revision 3 (one write)", got, e.Revision)
	}
}

func TestPlayerManagedKeepsQueueOnInitiativeChange(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10))
	e, _ = s.UpdateSettings(ctx, "u1", "u1", e.ID, e.Revision, map[string]any{"ordering": data.OrderPlayerManaged})
	b := findByName(e, "B")
	e, err := s.UpdateCombatant(ctx, "u1", "u1", e.ID, e.Revision, b.ID, CombatantPatch{Initiative: f(99)})
	if err != nil || !slices.Equal(names(e, e.TurnQueue), []string{"A", "B"}) {
		t.Fatalf("err=%v queue=%v, want A,B kept", err, names(e, e.TurnQueue))
	}
}

func TestTurnActionsAreOwnerScoped(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20))
	if _, err := s.Advance(ctx, "u2", "u2", e.ID, e.Revision); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestRemovingCurrentInTheMiddleKeepsRound(t *testing.T) {
	s := newTurnService(t)
	e := startedEncounter(t, s, scored("A", data.KindPC, 20), scored("B", data.KindPC, 10), scored("C", data.KindPC, 5))
	a := findByName(e, "A")
	e, err := s.RemoveCombatant(ctx, "u1", "u1", e.ID, e.Revision, a.ID)
	if err != nil || findByID(e, e.CurrentCombatantID).Name != "B" || e.Round != 1 {
		t.Fatalf("err=%v current=%q round=%d, want B in round 1", err, e.CurrentCombatantID, e.Round)
	}
}

func noSystemService(t *testing.T) *Encounters {
	t.Helper()
	s := newTurnService(t)
	zero := 0
	none := data.GameSystem{ID: data.DefaultSystemID, Name: "No System", Family: "simple",
		Fields: []data.TrackerField{{Key: "health", Label: "Health", Kind: data.FieldCounter, Def: 1, Min: &zero}}}
	if _, err := s.systems.Upsert(ctx, none); err != nil {
		t.Fatal(err)
	}
	return s
}

func mixedKinds() []data.Combatant {
	return []data.Combatant{scored("Goblin", data.KindAdversary, 20), scored("Pia", data.KindPC, 3), scored("Pat", data.KindPC, 9)}
}

func TestNoSystemOrdersByInitiativeOnlyEvenWithPCVsAdversary(t *testing.T) {
	s := noSystemService(t)
	e, err := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: data.DefaultSystemID,
		Ordering: data.OrderPCVsAdversary, TieBreaker: data.TieQuery, Combatants: mixedKinds()})
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || !slices.Equal(names(e, e.TurnQueue), []string{"Goblin", "Pat", "Pia"}) {
		t.Fatalf("err=%v queue=%v, want initiative only: Goblin, Pat, Pia", err, names(e, e.TurnQueue))
	}
}

func TestPCVsAdversaryGroupsOnOtherSystems(t *testing.T) {
	s := noSystemService(t)
	e, err := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: "test",
		Ordering: data.OrderPCVsAdversary, TieBreaker: data.TieQuery, Combatants: mixedKinds()})
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	if err != nil || !slices.Equal(names(e, e.TurnQueue), []string{"Pat", "Pia", "Goblin"}) {
		t.Fatalf("err=%v queue=%v, want PCs then adversary", err, names(e, e.TurnQueue))
	}
}

func TestSwitchingSystemRecomputesTheQueue(t *testing.T) {
	s := noSystemService(t)
	e, _ := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: "test",
		Ordering: data.OrderPCVsAdversary, TieBreaker: data.TieQuery, Combatants: mixedKinds()})
	e, _ = s.Start(ctx, "u1", "u1", e.ID, e.Revision)
	e, err := s.UpdateSettings(ctx, "u1", "u1", e.ID, e.Revision, map[string]any{"game_system_id": data.DefaultSystemID})
	if err != nil || !slices.Equal(names(e, e.TurnQueue), []string{"Goblin", "Pat", "Pia"}) {
		t.Fatalf("err=%v queue=%v, want initiative only after switching to No System", err, names(e, e.TurnQueue))
	}
	e, err = s.UpdateSettings(ctx, "u1", "u1", e.ID, e.Revision, map[string]any{"game_system_id": "test"})
	if err != nil || !slices.Equal(names(e, e.TurnQueue), []string{"Pat", "Pia", "Goblin"}) {
		t.Fatalf("err=%v queue=%v, want grouping back on another system", err, names(e, e.TurnQueue))
	}
}
