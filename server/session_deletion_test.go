package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	other := buildEncounter("5f00000000000000000000e7", foreignSession)
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

// endSessionRel is roleChecks, except that DELETE /authz/sessions/{id} answers statuses in turn
// (the last one repeats) and counts its calls.
func endSessionRel(calls *atomic.Int64, statuses ...int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/authz/sessions/") {
			n := int(calls.Add(1))
			w.WriteHeader(statuses[min(n, len(statuses))-1])
			return
		}
		roleChecks(w, r)
	}
}

func TestADeletionThatCannotEndTheSessionAtAuthAPIFailsAndTheNextOneEndsItAgain(t *testing.T) {
	var ends atomic.Int64
	r, db, sid := viewTableRel(t, "3", func(d *fake.DB) store.Database { return d }, endSessionRel(&ends, http.StatusServiceUnavailable, http.StatusNoContent))

	first := deleteSession(r, "user-ok", sid, "1")
	retry := deleteSession(r, "user-ok", sid, "1")

	if first.Code < 500 {
		t.Fatalf("delete while auth-api cannot end the session = %d %s, want an error: the mint and tuples are still there", first.Code, first.Body)
	}
	if retry.Code != http.StatusNoContent || ends.Load() != 2 {
		t.Fatalf("retry = %d %s after %d end calls; want 204 after ending the session a second time", retry.Code, retry.Body, ends.Load())
	}
	if storedEncounter(t, db, encE1).SessionID != "" {
		t.Error("the encounter was not released by the retry")
	}
}

func TestARetryAfterTheSessionWasEndedTreatsADenialAsAlreadyEnded(t *testing.T) {
	var ends atomic.Int64
	var failEncounters atomic.Bool
	r, db, sid := viewTableRel(t, "3", func(d *fake.DB) store.Database { return failingEncounters{d, &failEncounters} },
		endSessionRel(&ends, http.StatusNoContent, http.StatusForbidden)) // the second call finds the GM tuple gone

	failEncounters.Store(true)
	first := deleteSession(r, "user-ok", sid, "1")
	failEncounters.Store(false)
	retry := deleteSession(r, "user-ok", sid, "1")

	if first.Code < 500 || retry.Code != http.StatusNoContent {
		t.Fatalf("first = %d, retry = %d %s; want an error then 204", first.Code, retry.Code, retry.Body)
	}
	if storedEncounter(t, db, encE1).SessionID != "" {
		t.Error("the encounter was not released by the retry")
	}
}

func TestReleasedEncountersCannotLeaveTheGMOverTheSavedEncounterLimit(t *testing.T) {
	r, _, sid := changeTable(t) // every entitlement limit is 3
	for i := 0; i < 3; i++ {
		newEncounter(t, r, "user-ok", `{"name":"P","game_system_id":"`+data.DefaultSystemID+`"}`)
	}

	w := deleteSession(r, "user-ok", sid, "1")

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	inactive := 0
	for _, it := range decodeBody(t, callAs(r, "user-ok", "GET", "/encounters", ""))["items"].([]any) {
		if _, off := it.(map[string]any)["inactive_at"]; off {
			inactive++
		}
	}
	if inactive != 1 {
		t.Fatalf("%d inactive encounters of 4 (limit 3), want exactly 1: the release must reconcile the GM's limit", inactive)
	}
}
