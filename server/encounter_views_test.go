package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	foreignSession = "5f00000000000000000000aa" // a session none of the test users belongs to
	secretName     = "Assassin Vex"
	secretNote     = "SECRET plan"
)

// roleChecks is an auth-api whose check-relation answers by bearer token the way OpenFGA would:
// user-ok is the GM, user-two a player, user-three an observer, and nobody belongs to
// foreignSession. Everything else (session mint, tuple writes) is allowed.
func roleChecks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/authz/check-relation" {
		allowRelations(w, r)
		return
	}
	var b struct{ Object, Relation string }
	_ = json.NewDecoder(r.Body).Decode(&b)
	role := map[string]string{"Bearer user-ok": "gm", "Bearer user-two": "player", "Bearer user-three": "observer", "Bearer user-four": "co_gm"}[r.Header.Get("Authorization")]
	if strings.HasSuffix(b.Object, foreignSession) {
		role = ""
	}
	allowed := false
	switch b.Relation {
	case "can_view":
		allowed = role != ""
	case "can_edit", "can_approve":
		allowed = role == "gm" || role == "co_gm"
	default:
		allowed = role == b.Relation
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
}

// viewTable is a session with user-ok as GM, user-two as a player (u-2) and user-three as an
// observer (u-3), plus one stored encounter holding a hidden adversary that is the current turn.
func viewTable(t *testing.T) (r http.Handler, db *fake.DB, sessionID string) {
	t.Helper()
	return viewTableLimit(t, "3")
}

// viewTableLimit is viewTable with every entitlement limit set to limit.
func viewTableLimit(t *testing.T, limit string) (r http.Handler, db *fake.DB, sessionID string) {
	t.Helper()
	return viewTableOn(t, limit, func(d *fake.DB) store.Database { return d })
}

// viewTableOn is viewTableLimit with the router's database wrapped by wrap; the returned db is the
// underlying fake, for seeding and inspection.
func viewTableOn(t *testing.T, limit string, wrap func(*fake.DB) store.Database) (r http.Handler, db *fake.DB, sessionID string) {
	t.Helper()
	db = fake.NewDB()
	router, _ := credRouterRel(t, limit, wrap(db), roleChecks)
	w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	sessionID = decodeBody(t, w)["id"].(string)
	for token, role := range map[string]string{"user-two": "player", "user-three": "observer"} {
		code := inviteCode(t, router, "user-ok", sessionID)
		jw := callAs(router, token, "POST", "/sessions/join", `{"code":"`+code+`"}`)
		if jw.Code != http.StatusAccepted {
			t.Fatalf("join = %d %s", jw.Code, jw.Body)
		}
		reqID := decodeBody(t, jw)["id"].(string)
		if aw := callAs(router, "user-ok", "POST", "/sessions/"+sessionID+"/requests/"+reqID+"/approve", `{"role":"`+role+`"}`); aw.Code != 200 {
			t.Fatalf("approve = %d %s", aw.Code, aw.Body)
		}
	}
	seedEncounter(t, db, "5f00000000000000000000e1", sessionID)
	return router, db, sessionID
}

func seedEncounter(t *testing.T, db *fake.DB, id, session string) {
	t.Helper()
	if err := db.Fake(data.CollectionEncounters).InsertOne(context.Background(), buildEncounter(id, session)); err != nil {
		t.Fatal(err)
	}
}

// replaceEncounter stores edit's result in place of the encounter, as a committed edit would.
func replaceEncounter(t *testing.T, db *fake.DB, id, session string, edit func(*data.Encounter)) {
	t.Helper()
	enc := buildEncounter(id, session)
	edit(&enc)
	c := db.Fake(data.CollectionEncounters)
	if err := c.DeleteOne(context.Background(), bson.D{{Key: "_id", Value: id}}); err != nil {
		t.Fatal(err)
	}
	if err := c.InsertOne(context.Background(), enc); err != nil {
		t.Fatal(err)
	}
}

func buildEncounter(id, session string) data.Encounter {
	init := 14.0
	enc := data.Encounter{
		Meta: data.Meta{ID: id, OwnerUserID: "u-1", Revision: 3}, Name: "Ambush", GameSystemID: "d20", Status: data.StatusActive,
		Round: 1, SessionID: session, CurrentCombatantID: "hid", TurnQueue: []string{"hid", "pc", "orc"},
		Combatants: []data.Combatant{
			{ID: "pc", Name: "Aria", Kind: data.KindPC, ControllerUserIDs: []string{"u-2"}, Health: map[string]int{"current": 9}, Notes: "shared", NotesShared: true},
			{ID: "hid", Name: secretName, Kind: data.KindAdversary, Hidden: true, Initiative: &init, Notes: secretNote, Health: map[string]int{"current": 31}, HealthVisibility: data.HealthFull},
			{ID: "orc", Name: "Orc", Kind: data.KindAdversary, Notes: "GM only orc note", Health: map[string]int{"current": 5}, HealthVisibility: data.HealthFull},
		},
	}
	enc.CreatedAt, enc.UpdatedAt = time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 0)
	return enc
}

func encPath(session, id string) string { return "/sessions/" + session + "/encounters/" + id }

func TestPlayersNeverReceiveHiddenDataOverREST(t *testing.T) {
	r, _, sid := viewTable(t)
	paths := []string{"/sessions/" + sid + "/encounters", encPath(sid, "5f00000000000000000000e1")}

	for _, token := range []string{"user-two", "user-three"} {
		for _, path := range paths {
			w := callAs(r, token, "GET", path, "")

			if w.Code != 200 {
				t.Fatalf("%s GET %s = %d %s", token, path, w.Code, w.Body)
			}
			for _, secret := range []string{secretName, secretNote, `"hid"`, "GM only orc note", "owner_user_id", `"revision"`, "updated_at", "initiative"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Errorf("%s GET %s leaks %q: %s", token, path, secret, w.Body)
				}
			}
		}
	}
}

func TestPlayerViewHasTurnQueueAndCountsOfVisibleCombatantsOnly(t *testing.T) {
	r, _, sid := viewTable(t)

	body := decodeBody(t, callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e1"), ""))

	if q := body["turn_queue"].([]any); len(q) != 2 || q[0] != "pc" || q[1] != "orc" {
		t.Errorf("turn_queue = %v, want [pc orc]", q)
	}
	if body["combatant_count"] != float64(2) || len(body["combatants"].([]any)) != 2 {
		t.Errorf("count = %v combatants = %v, want 2", body["combatant_count"], body["combatants"])
	}
	if _, has := body["current_combatant_id"]; has {
		t.Errorf("current_combatant_id = %v, want none while a hidden combatant is current", body["current_combatant_id"])
	}
}

func TestGMSeesEverythingOverREST(t *testing.T) {
	r, _, sid := viewTable(t)

	w := callAs(r, "user-ok", "GET", encPath(sid, "5f00000000000000000000e1"), "")
	body := decodeBody(t, w)

	if !strings.Contains(w.Body.String(), secretName) || !strings.Contains(w.Body.String(), secretNote) || body["revision"] != float64(3) {
		t.Fatalf("GM view = %s, want hidden combatant, its notes and the revision", w.Body)
	}
	if body["combatant_count"] != float64(3) || len(body["turn_queue"].([]any)) != 3 {
		t.Errorf("GM count=%v queue=%v, want all three", body["combatant_count"], body["turn_queue"])
	}
}

func TestAHiddenCombatantAndAMissingOneAreIndistinguishable(t *testing.T) {
	r, _, sid := viewTable(t)

	hidden := callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e1")+"/combatants/hid", "")
	missing := callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e1")+"/combatants/nope", "")
	gm := callAs(r, "user-ok", "GET", encPath(sid, "5f00000000000000000000e1")+"/combatants/hid", "")

	if hidden.Code != 404 || missing.Code != 404 || normalizeErr(hidden.Body.String()) != normalizeErr(missing.Body.String()) {
		t.Fatalf("hidden = %d %s, missing = %d %s; want the same 404", hidden.Code, hidden.Body, missing.Code, missing.Body)
	}
	if gm.Code != 200 {
		t.Errorf("GM GET hidden combatant = %d, want 200", gm.Code)
	}
}

// normalizeErr drops the per-request correlation id so two error bodies compare equal.
func normalizeErr(body string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	delete(m, "correlation_id")
	b, _ := json.Marshal(m)
	return string(b)
}

func TestPlayerFetchesACombatantWithGMOnlyNotes(t *testing.T) {
	r, _, sid := viewTable(t)

	w := callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e1")+"/combatants/orc", "")

	if w.Code != 200 || strings.Contains(w.Body.String(), "GM only orc note") || strings.Contains(w.Body.String(), `"notes"`) {
		t.Fatalf("player GET combatant = %d %s, want the combatant with no notes", w.Code, w.Body)
	}
}

func TestNonMembersGet403BeforeAnythingIsLookedUp(t *testing.T) {
	r, db, _ := viewTable(t)
	seedEncounter(t, db, "5f00000000000000000000e2", foreignSession)

	for _, path := range []string{"/sessions/" + foreignSession + "/encounters", encPath(foreignSession, "5f00000000000000000000e2"),
		encPath(foreignSession, "5f00000000000000000000e2") + "/combatants/pc", encPath(foreignSession, "5f0000000000000000000099")} {
		w := callAs(r, "user-two", "GET", path, "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a member of this session") || strings.Contains(w.Body.String(), "own this record") {
			t.Errorf("GET %s = %d %s, want 403 saying the caller is not a member, whether or not the encounter exists", path, w.Code, w.Body)
		}
	}
}

func TestUnknownEncounterAndAnotherSessionsEncounterAreTheSame404(t *testing.T) {
	r, db, sid := viewTable(t)
	seedEncounter(t, db, "5f00000000000000000000e3", foreignSession)

	other := callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e3"), "")
	unknown := callAs(r, "user-two", "GET", encPath(sid, "5f00000000000000000000e9"), "")
	malformed := callAs(r, "user-two", "GET", encPath(sid, "nope"), "")

	for name, w := range map[string]*httptest.ResponseRecorder{"other session": other, "unknown": unknown, "malformed": malformed} {
		if w.Code != 404 || normalizeErr(w.Body.String()) != normalizeErr(unknown.Body.String()) {
			t.Errorf("%s = %d %s, want the same 404 as an unknown id", name, w.Code, w.Body)
		}
	}
}

func TestETagOfAPlayerViewIgnoresHiddenEditsButFollowsVisibleOnes(t *testing.T) {
	r, db, sid := viewTable(t)
	path := encPath(sid, "5f00000000000000000000e1")
	before := callAs(r, "user-two", "GET", path, "")
	gmBefore := callAs(r, "user-ok", "GET", path, "")

	replaceEncounter(t, db, "5f00000000000000000000e1", sid, func(e *data.Encounter) {
		e.Revision, e.UpdatedAt = 9, e.UpdatedAt.Add(time.Hour)
		e.Combatants[1].Name, e.Combatants[1].Notes = "Renamed", "changed"
	})
	after := callAs(r, "user-two", "GET", path, "")
	gmAfter := callAs(r, "user-ok", "GET", path, "")
	replaceEncounter(t, db, "5f00000000000000000000e1", sid, func(e *data.Encounter) { e.Combatants[0].Name = "Aria the Bold" })
	changed := callAs(r, "user-two", "GET", path, "")

	if before.Header().Get("ETag") == "" || before.Header().Get("ETag") != after.Header().Get("ETag") || before.Body.String() != after.Body.String() {
		t.Errorf("player ETag %q -> %q after a hidden-only edit, want unchanged", before.Header().Get("ETag"), after.Header().Get("ETag"))
	}
	if changed.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Error("player ETag did not change after a visible edit")
	}
	if gmBefore.Header().Get("ETag") == gmAfter.Header().Get("ETag") {
		t.Error("GM ETag did not follow the revision")
	}
}

func TestOwnerReadsPrivateEncountersAndSessionOnesGoThroughTheSession(t *testing.T) {
	r, db, sid := viewTable(t)
	seedPrivate := data.Encounter{Meta: data.Meta{ID: "5f00000000000000000000f1", OwnerUserID: "u-1", Revision: 1}, Name: "Solo", GameSystemID: "d20", Status: data.StatusDraft,
		Combatants: []data.Combatant{{ID: "a", Name: "Goblin", Kind: data.KindAdversary, Hidden: true}}}
	if err := db.Fake(data.CollectionEncounters).InsertOne(context.Background(), seedPrivate); err != nil {
		t.Fatal(err)
	}

	own := callAs(r, "user-ok", "GET", "/encounters/5f00000000000000000000f1", "")
	list := callAs(r, "user-ok", "GET", "/encounters", "")
	viaOwner := callAs(r, "user-ok", "GET", "/encounters/5f00000000000000000000e1", "")
	otherUser := callAs(r, "user-two", "GET", "/encounters/5f00000000000000000000f1", "")
	_ = sid

	if own.Code != 200 || !strings.Contains(own.Body.String(), "Goblin") {
		t.Errorf("owner GET private = %d %s, want the whole encounter", own.Code, own.Body)
	}
	if items := decodeBody(t, list)["items"].([]any); len(items) != 1 {
		t.Errorf("owner list = %d items, want only the private encounter", len(items))
	}
	if viaOwner.Code != http.StatusForbidden {
		t.Errorf("owner GET of a session encounter = %d, want 403 (read it through the session)", viaOwner.Code)
	}
	if otherUser.Code != http.StatusForbidden {
		t.Errorf("another user GET = %d, want 403", otherUser.Code)
	}
}
