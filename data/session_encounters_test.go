package data

import (
	"errors"
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

var (
	gmDecision     = Decision{actor: "u-1", session: testID("s"), relation: relEdit}
	viewDecision   = Decision{actor: "u-2", session: testID("s"), relation: relView}
	otherSessionID = testID("t")
)

// seedSessionEncounter stores an encounter in the session (the data layer has no create-in-session
// yet, so tests insert it directly) with controllers on its two combatants.
func seedSessionEncounter(t *testing.T, r *Encounters, session string, controllers ...[]string) Encounter {
	t.Helper()
	e := Encounter{Meta: Meta{ID: testID("e"), OwnerUserID: "u-1", Revision: 1}, Name: "Ambush", SessionID: session,
		Status: StatusDraft, Ordering: OrderHighToLow, TieBreaker: TieRandom,
		Combatants: []Combatant{{ID: "a", Name: "A", Kind: KindPC}, {ID: "b", Name: "B", Kind: KindPC}}}
	for i, c := range controllers {
		e.Combatants[i].ControllerUserIDs = c
	}
	if err := r.c.InsertOne(ctx, e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestUpdateInSessionWritesUnderTheRevisionAndBumpsIt(t *testing.T) {
	r := newEncounters(t)
	e := seedSessionEncounter(t, r, gmDecision.session)
	cs := slices.Clone(e.Combatants)
	cs[0].ControllerUserIDs = []string{"u-2"}

	got, err := r.UpdateInSession(ctx, gmDecision, e.ID, 1, map[string]any{"combatants": cs})

	if err != nil || got.Revision != 2 || got.UpdatedBy != "u-1" || !slices.Equal(got.Combatants[0].ControllerUserIDs, []string{"u-2"}) {
		t.Fatalf("got %+v, %v; want revision 2 by u-1 with the controller set", got, err)
	}
}

func TestUpdateInSessionClassifiesFailures(t *testing.T) {
	r := newEncounters(t)
	e := seedSessionEncounter(t, r, gmDecision.session)
	cs := map[string]any{"combatants": e.Combatants}
	other := Decision{actor: "u-1", session: otherSessionID, relation: relEdit}

	_, stale := r.UpdateInSession(ctx, gmDecision, e.ID, 9, cs)
	cur, _ := r.UpdateInSession(ctx, gmDecision, e.ID, 9, cs)
	_, wrongSession := r.UpdateInSession(ctx, other, e.ID, 1, cs)
	_, missing := r.UpdateInSession(ctx, gmDecision, testID("z"), 1, cs)
	_, noEdit := r.UpdateInSession(ctx, viewDecision, e.ID, 1, cs)
	_, badKey := r.UpdateInSession(ctx, gmDecision, e.ID, 1, map[string]any{"name": "x"})
	_, badBody := r.UpdateInSession(ctx, gmDecision, e.ID, 1, map[string]any{"combatants": []Combatant{{ID: "x"}}})

	checks := map[string][2]error{
		"stale revision": {stale, ErrRevisionConflict}, "another session": {wrongSession, ErrNotFound},
		"missing": {missing, ErrNotFound}, "view-only decision": {noEdit, ErrForbidden},
		"field other than combatants": {badKey, ErrInvalid}, "invalid combatant": {badBody, ErrInvalid},
	}
	for name, c := range checks {
		if !errors.Is(c[0], c[1]) {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	if cur.Revision != 1 {
		t.Errorf("a conflict returns the current record at revision 1, got %d", cur.Revision)
	}
}

func TestUpdateInSessionNeverTouchesAPrivateEncounter(t *testing.T) {
	r := newEncounters(t)
	e := seedSessionEncounter(t, r, "")

	_, err := r.UpdateInSession(ctx, gmDecision, e.ID, 1, map[string]any{"combatants": e.Combatants})

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("session write on a private encounter = %v, want ErrNotFound", err)
	}
}

func TestScrubControllerRemovesOnlyThatUserEverywhereInTheSession(t *testing.T) {
	r := newEncounters(t)
	e := seedSessionEncounter(t, r, gmDecision.session, []string{"u-2", "u-3"}, []string{"u-2"})

	if err := r.ScrubController(ctx, gmDecision, "u-2"); err != nil {
		t.Fatal(err)
	}

	var got Encounter
	if err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: e.ID}}, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Combatants[0].ControllerUserIDs, []string{"u-3"}) || len(got.Combatants[1].ControllerUserIDs) != 0 || got.Revision != 2 {
		t.Fatalf("controllers = %v / %v, revision %d; want [u-3], none, 2", got.Combatants[0].ControllerUserIDs, got.Combatants[1].ControllerUserIDs, got.Revision)
	}
}

func TestScrubControllerLeavesOtherSessionsAndUntouchedEncountersAlone(t *testing.T) {
	r := newEncounters(t)
	e := seedSessionEncounter(t, r, otherSessionID, []string{"u-2"})

	if err := r.ScrubController(ctx, gmDecision, "u-2"); err != nil {
		t.Fatal(err)
	}
	if err := r.ScrubController(ctx, gmDecision, "u-2"); err != nil { // idempotent
		t.Fatal(err)
	}

	var got Encounter
	_ = r.c.FindOne(ctx, bson.D{{Key: "_id", Value: e.ID}}, &got)
	if !slices.Equal(got.Combatants[0].ControllerUserIDs, []string{"u-2"}) || got.Revision != 1 {
		t.Fatalf("another session's encounter changed: %v revision %d", got.Combatants[0].ControllerUserIDs, got.Revision)
	}
}

func TestScrubControllerAuthority(t *testing.T) {
	r := newEncounters(t)
	seedSessionEncounter(t, r, gmDecision.session, []string{"u-2"})

	own := r.ScrubController(ctx, viewDecision, "u-2")    // a member scrubbing their own leave
	others := r.ScrubController(ctx, viewDecision, "u-3") // a view-only decision scrubbing someone else

	if own != nil || !errors.Is(others, ErrForbidden) {
		t.Fatalf("own = %v, others = %v; want nil and ErrForbidden", own, others)
	}
}
