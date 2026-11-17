package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"go.mongodb.org/mongo-driver/bson"
)

const newSessionEncounter = `{"name":"Ambush","game_system_id":"` + data.DefaultSystemID + `"}`

func createIn(r http.Handler, token, sid string) *httptest.ResponseRecorder {
	return callAs(r, token, "POST", "/sessions/"+sid+"/encounters", newSessionEncounter)
}

func sessionRevision(t *testing.T, r http.Handler, sid string) string {
	t.Helper()
	return strconv.Itoa(int(decodeBody(t, callAs(r, "user-ok", "GET", "/sessions/"+sid, ""))["revision"].(float64)))
}

// setCoGMCreate has token PATCH the co-GM encounter-creation setting at the session's current revision.
func setCoGMCreate(t *testing.T, r http.Handler, token, sid string, on bool) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(r, token, "PATCH", "/sessions/"+sid, `{"allow_co_gm_create_encounter":`+strconv.FormatBool(on)+`,"revision":`+sessionRevision(t, r, sid)+`}`)
}

func TestTheGMCreatesAnEncounterInTheSessionAndOwnsIt(t *testing.T) {
	r, db, sid := viewTable(t)
	seedSystem(t, db)

	w := createIn(r, "user-ok", sid)

	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	e := storedEncounter(t, db, decodeBody(t, w)["id"].(string))
	if e.SessionID != sid || e.OwnerUserID != "u-1" {
		t.Errorf("stored session %q owner %q, want the session and the GM", e.SessionID, e.OwnerUserID)
	}
}

func TestACoGMCreatesOnlyWhileTheGMAllowsItAndLosesItAtOnce(t *testing.T) {
	r, db, sid := viewTable(t)
	seedSystem(t, db)
	joinAs(t, r, sid, "user-four", "co_gm")

	off := createIn(r, "user-four", sid)
	if on := setCoGMCreate(t, r, "user-ok", sid, true); on.Code != http.StatusOK {
		t.Fatalf("GM enabling = %d %s", on.Code, on.Body)
	}
	allowed := createIn(r, "user-four", sid)
	if again := setCoGMCreate(t, r, "user-ok", sid, false); again.Code != http.StatusOK {
		t.Fatalf("GM disabling = %d %s", again.Code, again.Body)
	}
	revoked := createIn(r, "user-four", sid)

	if off.Code != http.StatusForbidden || revoked.Code != http.StatusForbidden {
		t.Errorf("setting off = %d, after turning it off again = %d, want 403 both", off.Code, revoked.Code)
	}
	if allowed.Code != http.StatusCreated {
		t.Fatalf("co-GM with the setting on = %d %s", allowed.Code, allowed.Body)
	}
	if e := storedEncounter(t, db, decodeBody(t, allowed)["id"].(string)); e.OwnerUserID != "u-1" || e.SessionID != sid {
		t.Errorf("co-GM's encounter owner %q session %q, want the GM and the session", e.OwnerUserID, e.SessionID)
	}
}

func TestOnlyTheGMChangesTheSettingAndOnlyTheGMAndCoGMsSeeIt(t *testing.T) {
	r, _, sid := viewTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")

	byCoGM := setCoGMCreate(t, r, "user-four", sid, true)
	byPlayer := setCoGMCreate(t, r, "user-two", sid, true)
	byGM := setCoGMCreate(t, r, "user-ok", sid, true)
	unknown := callAs(r, "user-ok", "PATCH", "/sessions/"+sid, `{"revision":1,"bogus":true}`)

	if byCoGM.Code != http.StatusForbidden || byPlayer.Code != http.StatusForbidden {
		t.Errorf("co-GM = %d, player = %d, want 403 both", byCoGM.Code, byPlayer.Code)
	}
	if byGM.Code != http.StatusOK || decodeBody(t, byGM)["allow_co_gm_create_encounter"] != true {
		t.Errorf("GM = %d %s, want 200 with the setting on", byGM.Code, byGM.Body)
	}
	if unknown.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown field = %d, want 422", unknown.Code)
	}
	for token, want := range map[string]bool{"user-ok": true, "user-four": true, "user-two": false, "user-three": false} {
		_, has := decodeBody(t, callAs(r, token, "GET", "/sessions/"+sid, ""))["allow_co_gm_create_encounter"]
		if has != want {
			t.Errorf("%s sees the setting = %v, want %v", token, has, want)
		}
	}
}

func TestCreatingASessionCanTurnTheSettingOn(t *testing.T) {
	r, _, _ := viewTable(t)

	w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"Open","allow_co_gm_create_encounter":true}`)

	if w.Code != http.StatusCreated || decodeBody(t, w)["allow_co_gm_create_encounter"] != true {
		t.Fatalf("create = %d %s, want 201 with the setting on", w.Code, w.Body)
	}
}

func TestPlayersAndObserversCannotCreateEvenWithTheSettingOn(t *testing.T) {
	r, db, sid := viewTable(t)
	setCoGMCreate(t, r, "user-ok", sid, true)

	player, observer := createIn(r, "user-two", sid), createIn(r, "user-three", sid)

	if player.Code != http.StatusForbidden || observer.Code != http.StatusForbidden {
		t.Errorf("player = %d, observer = %d, want 403 both", player.Code, observer.Code)
	}
	if n, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), nil); n != 1 {
		t.Errorf("%d encounters stored, want only the fixture's", n)
	}
}

func TestSessionEncountersCountAgainstTheGMsLimitForGMAndCoGMAlike(t *testing.T) {
	r, db, sid := viewTableLimit(t, "2") // the fixture's session encounter is the first
	seedSystem(t, db)
	joinAs(t, r, sid, "user-four", "co_gm")
	setCoGMCreate(t, r, "user-ok", sid, true)

	second := createIn(r, "user-ok", sid)
	third := createIn(r, "user-four", sid)

	if second.Code != http.StatusCreated {
		t.Fatalf("second = %d %s", second.Code, second.Body)
	}
	if b := decodeBody(t, third); third.Code != http.StatusForbidden || b["code"] != "entitlement_limit_reached" {
		t.Errorf("co-GM over the GM's limit = %d %s, want 403 entitlement_limit_reached", third.Code, third.Body)
	}
}

func TestACoGMCannotCreateWithoutTheGMsRecordedEntitlements(t *testing.T) {
	r, db, sid := viewTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")
	setCoGMCreate(t, r, "user-ok", sid, true)
	var sess data.Session
	if err := db.Fake(data.CollectionSessions).FindOneAndUpdate(t.Context(), bson.D{{Key: "_id", Value: sid}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "owner_limits", Value: ""}}}}, &sess); err != nil {
		t.Fatal(err)
	}

	w := createIn(r, "user-four", sid)

	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "owner_entitlements_pending" {
		t.Errorf("co-GM with no recorded entitlements = %d %s, want 503 owner_entitlements_pending", w.Code, w.Body)
	}
}

func TestCreatingInAnotherSessionOrWithACredentialIsRefused(t *testing.T) {
	r, db, sid := viewTable(t)
	_, secret := issue(t, r, "user-ok", `["read","write"]`)

	foreign := createIn(r, "user-ok", foreignSession)
	cred := createIn(r, secret, sid)

	if foreign.Code != http.StatusForbidden {
		t.Errorf("a session the caller does not belong to = %d, want 403", foreign.Code)
	}
	if cred.Code != http.StatusForbidden && cred.Code != http.StatusUnauthorized {
		t.Errorf("integration credential = %d, want it refused", cred.Code)
	}
	if n, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), nil); n != 1 {
		t.Errorf("%d encounters stored, want only the fixture's", n)
	}
}
