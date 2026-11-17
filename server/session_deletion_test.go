package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

func sessionRevision(t *testing.T, r http.Handler, sid string) string {
	t.Helper()
	return strconv.Itoa(int(decodeBody(t, callAs(r, "user-ok", "GET", "/sessions/"+sid, ""))["revision"].(float64)))
}

func deleteSession(r http.Handler, token, sid, rev string) *httptest.ResponseRecorder {
	return callAs(r, token, "DELETE", "/sessions/"+sid+"?revision="+rev, "")
}

func storedEncounter(t *testing.T, db *fake.DB, id string) data.Encounter {
	t.Helper()
	var e data.Encounter
	if err := db.Fake(data.CollectionEncounters).FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeletingASessionMakesItsEncountersPrivateEncountersOfTheGM(t *testing.T) {
	r, db, sid := changeTable(t)
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.OwnerUserID = "u-4" }) // not the GM, to show the owner changes
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	before := storedEncounter(t, db, encE1)

	w := deleteSession(r, "user-ok", sid, "1")

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	after := storedEncounter(t, db, encE1)
	if after.SessionID != "" || after.OwnerUserID != "u-1" || after.Revision != before.Revision+1 {
		t.Fatalf("encounter session %q owner %q revision %d; want no session, owner u-1, revision %d", after.SessionID, after.OwnerUserID, after.Revision, before.Revision+1)
	}
	for _, c := range after.Combatants {
		if len(c.ControllerUserIDs) != 0 {
			t.Errorf("combatant %s still lists controllers %v", c.ID, c.ControllerUserIDs)
		}
	}
	// The GM now reads it as their own private encounter, through the owner path.
	if g := callAs(r, "user-ok", "GET", "/encounters/"+encE1, ""); g.Code != 200 {
		t.Errorf("GM GET /encounters/e1 = %d %s, want it readable as their own", g.Code, g.Body)
	}
	if list := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters", "")); !containsID(list["items"], encE1) {
		t.Errorf("the GM's encounter list lacks the released encounter: %v", list["items"])
	}
	// Pending change requests are closed.
	var rq data.ChangeRequest
	if err := db.Fake(data.CollectionChangeRequests).FindOne(context.Background(), bson.D{{Key: "_id", Value: idOf(req)}}, &rq); err != nil || rq.Status != data.ChangeCancelled {
		t.Errorf("change request = %q (err %v), want cancelled", rq.Status, err)
	}
}

func containsID(items any, id string) bool {
	list, _ := items.([]any)
	for _, it := range list {
		if m, _ := it.(map[string]any); m["id"] == id {
			return true
		}
	}
	return false
}

func TestFormerMembersAndStrangersGetNothingFromAReleasedEncounter(t *testing.T) {
	r, _, sid := changeTable(t)
	deleteSession(r, "user-ok", sid, "1")

	for _, token := range []string{"user-two", "user-three", "user-four"} {
		for _, path := range []string{"/encounters/" + encE1, encPath(sid, encE1)} {
			if w := callAs(r, token, "GET", path, ""); w.Code < 400 || w.Code == http.StatusInternalServerError {
				t.Errorf("%s GET %s = %d %s, want it refused", token, path, w.Code, w.Body)
			}
		}
		if w := callAs(r, token, "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":[],"revision":4}`); w.Code < 400 {
			t.Errorf("%s assigning controllers on a deleted session = %d", token, w.Code)
		}
	}
}

func TestOnlyTheGMCanDeleteAndNobodyElseCanResumeADeletion(t *testing.T) {
	r, db, sid := changeTable(t)

	for _, token := range []string{"user-two", "user-three", "user-four"} {
		if w := deleteSession(r, token, sid, "1"); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
			t.Errorf("%s deletes a live session = %d %s, want it refused", token, w.Code, w.Body)
		}
	}
	if e := storedEncounter(t, db, encE1); e.SessionID != sid {
		t.Fatal("a refused delete released the encounter")
	}

	// A different session's encounter, and the first session's once it is deleted, stay out of reach.
	other := buildEncounterAt("5f00000000000000000000e7", foreignSession)
	if err := db.Fake(data.CollectionEncounters).InsertOne(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	deleteSession(r, "user-ok", sid, "1")
	for _, token := range []string{"user-two", "user-three", "user-four"} {
		if w := deleteSession(r, token, sid, "1"); w.Code < 400 {
			t.Errorf("%s deletes (resumes) the GM's deleted session = %d", token, w.Code)
		}
	}
	if e := storedEncounter(t, db, other.ID); e.SessionID != foreignSession {
		t.Errorf("another session's encounter was released: %q", e.SessionID)
	}
}

func buildEncounterAt(id, session string) data.Encounter { return buildEncounter(id, session) }

func TestAStaleRevisionDoesNotReleaseAnything(t *testing.T) {
	r, db, sid := changeTable(t)

	w := deleteSession(r, "user-ok", sid, "99")

	if w.Code != http.StatusConflict || storedEncounter(t, db, encE1).SessionID != sid {
		t.Fatalf("delete at a stale revision = %d %s, want 409 and the encounter still in the session", w.Code, w.Body)
	}
}

func TestADeletionThatFailsPartwayIsFinishedByDeletingAgain(t *testing.T) {
	var fail atomic.Bool
	r, db, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database { return failingEncounters{d, &fail} })
	seedSystem(t, db)
	setPC(t, db, sid, func(*data.Encounter) {})
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

	fail.Store(true)
	first := deleteSession(r, "user-ok", sid, "1")
	fail.Store(false)

	if first.Code < 500 {
		t.Fatalf("delete with the encounter store failing = %d %s, want a server error", first.Code, first.Body)
	}
	if e := storedEncounter(t, db, encE1); e.SessionID != sid {
		t.Fatal("setup: the encounter should still be session-bound after the failed pass")
	}
	if g := callAs(r, "user-ok", "GET", "/sessions/"+sid, ""); g.Code < 400 {
		t.Fatalf("the session is still readable after its soft delete: %d", g.Code)
	}

	retry := deleteSession(r, "user-ok", sid, "1") // the revision is stale now; resuming does not need it
	again := deleteSession(r, "user-ok", sid, "1")

	if retry.Code != http.StatusNoContent || again.Code != http.StatusNoContent {
		t.Fatalf("retry = %d %s, repeat = %d %s; want 204 and 204", retry.Code, retry.Body, again.Code, again.Body)
	}
	e := storedEncounter(t, db, encE1)
	if e.SessionID != "" || e.OwnerUserID != "u-1" || e.Revision != 4 {
		t.Errorf("encounter session %q owner %q revision %d after the retry; want released once (revision 4)", e.SessionID, e.OwnerUserID, e.Revision)
	}
	var rq data.ChangeRequest
	if err := db.Fake(data.CollectionChangeRequests).FindOne(context.Background(), bson.D{{Key: "_id", Value: idOf(req)}}, &rq); err != nil || rq.Status != data.ChangeCancelled {
		t.Errorf("change request = %q (err %v), want cancelled", rq.Status, err)
	}
}

func TestDeletionLeavesSoftDeletedAndOtherUsersEncountersAlone(t *testing.T) {
	r, db, sid := changeTable(t)
	gone := buildEncounter("5f00000000000000000000e8", sid)
	gone.DeletedAt = ptrTime(time.Now())
	foreign := buildEncounter("5f00000000000000000000e9", foreignSession)
	for _, e := range []data.Encounter{gone, foreign} {
		if err := db.Fake(data.CollectionEncounters).InsertOne(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	deleteSession(r, "user-ok", sid, "1")

	if e := storedEncounter(t, db, gone.ID); e.SessionID != sid {
		t.Error("a soft-deleted encounter was resurrected as a private one")
	}
	if e := storedEncounter(t, db, foreign.ID); e.SessionID != foreignSession {
		t.Error("another session's encounter was released")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
