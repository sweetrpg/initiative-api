package data

import (
	"errors"
	"testing"
)

const otherSession = "5f0000000000000000000002"

func sessionEncounter(t *testing.T, r *Encounters, owner, session string, refs map[string]string) *Encounter {
	t.Helper()
	e := refEncounter(refs, false)
	e.SessionID = session
	if err := r.Create(ctx, owner, owner, e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFindByExternalRefNeverReturnsSessionEncounters(t *testing.T) {
	r := newEncounters(t)
	refs := map[string]string{"foundry.actor": "abc"}
	sessionEncounter(t, r, "u1", otherSession, refs)

	got, err := r.FindByExternalRef(ctx, "u1", "foundry.actor", "abc")

	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no matches from an encounter that belongs to a session (hidden combatants, GM-only notes)", got, err)
	}
}

func TestListOwnedViewsSkipsSessionEncountersAndLoadOwnedViewRefusesThem(t *testing.T) {
	r := newEncounters(t)
	private := newEncounter(t, r, "u1")
	inSession := sessionEncounter(t, r, "u1", otherSession, nil)

	page, v, err := r.ListOwnedViews(ctx, "u1", Query{})
	_, _, errSession := r.LoadOwnedView(ctx, "u1", inSession.ID)
	got, gv, errPrivate := r.LoadOwnedView(ctx, "u1", private.ID)

	if err != nil || len(page.Items) != 1 || page.Items[0].ID != private.ID || v.Level() != ViewGM || v.SessionID() != "" {
		t.Fatalf("list = %v, %v, %v; want only the private encounter and an owner viewer", page.Items, v, err)
	}
	if !errors.Is(errSession, ErrForbidden) {
		t.Errorf("owner load of a session encounter err = %v, want ErrForbidden", errSession)
	}
	if errPrivate != nil || got.ID != private.ID || gv.Level() != ViewGM {
		t.Errorf("owner load of a private encounter = %v, %v, %v", got.ID, gv, errPrivate)
	}
}

func TestLoadForViewerRefusesTheZeroViewer(t *testing.T) {
	r := newEncounters(t)
	e := sessionEncounter(t, r, "u1", otherSession, nil)

	_, errLoad := r.LoadForViewer(ctx, Viewer{}, e.ID)
	_, errList := r.ListForViewer(ctx, Viewer{}, 10)

	if !errors.Is(errLoad, ErrForbidden) || !errors.Is(errList, ErrForbidden) {
		t.Fatalf("load err=%v list err=%v, want ErrForbidden", errLoad, errList)
	}
}

func TestNotMemberIsAForbidden(t *testing.T) {
	if !errors.Is(ErrNotMember, ErrForbidden) {
		t.Fatal("ErrNotMember must satisfy errors.Is(ErrForbidden) so existing 403 handling still applies")
	}
}

func TestTheOwnerPathCannotReadOrChangeASessionEncounter(t *testing.T) {
	r := newEncounters(t)
	e := sessionEncounter(t, r, "u1", otherSession, nil)

	_, errGet := r.Get(ctx, "u1", e.ID)
	_, errLoad := r.Load(ctx, "u1", e.ID)
	_, errUpdate := r.Update(ctx, "u1", "u1", e.ID, 1, map[string]any{"name": "hijack"})
	errDelete := r.SoftDelete(ctx, "u1", "u1", e.ID, 1)
	page, _ := r.List(ctx, "u1", Query{})

	for name, err := range map[string]error{"get": errGet, "load": errLoad, "update": errUpdate, "delete": errDelete} {
		if err == nil || errors.Is(err, ErrRevisionConflict) {
			t.Errorf("owner %s of a session encounter err = %v, want a refusal that does not return the document", name, err)
		}
	}
	if len(page.Items) != 0 {
		t.Errorf("owner List returned %d session encounters", len(page.Items))
	}
	if after, err := r.c.CountDocuments(ctx, scopeID("u1", e.ID)); err != nil || after != 1 {
		t.Errorf("encounter count = %d, %v; want it untouched", after, err)
	}
}

func TestEncounterCountAndLimitStatesCoverPrivateEncountersOnly(t *testing.T) {
	r := newEncounters(t)
	newEncounter(t, r, "u1")
	sessionEncounter(t, r, "u1", otherSession, nil)

	n, err := r.Count(ctx, "u1", nil)
	states, serr := r.ItemStates(ctx, "u1")

	if err != nil || n != 1 || serr != nil || len(states) != 1 {
		t.Fatalf("count = %d, %v; item states = %d, %v; want 1: a session encounter counts against the session's limits", n, err, len(states), serr)
	}
}
