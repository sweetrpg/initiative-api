package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// encRouter is a router over a fresh database that holds one game system, "test", with a bounded
// hp counter, plus the database for tests that need to look underneath.
func encRouter(t *testing.T, limit string) (*gin.Engine, *fake.DB) {
	t.Helper()
	db := fake.NewDB()
	seedSystem(t, db)
	r, _ := credRouterDB(t, limit, db)
	return r, db
}

func seedSystem(t *testing.T, db *fake.DB) {
	t.Helper()
	zero, one := 0, 1
	sys := data.GameSystem{ID: data.DefaultSystemID, Name: "No System", Family: "f", Aliases: []string{"none"}, Fields: []data.TrackerField{
		{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 10, Min: &zero, MaxRef: "max_hp"},
		{Key: "max_hp", Label: "Max HP", Kind: data.FieldCounter, Def: 10, Min: &one},
	}}
	if _, err := data.NewSystemRepo(db).Upsert(context.Background(), sys); err != nil {
		t.Fatal(err)
	}
}

func revision(m map[string]any) int { return int(m["revision"].(float64)) }

func newEncounter(t *testing.T, r http.Handler, token, body string) map[string]any {
	t.Helper()
	w := callAs(r, token, "POST", "/encounters", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create encounter = %d %s", w.Code, w.Body)
	}
	return decodeBody(t, w)
}

const basicEncounter = `{"name":"Ambush","game_system_id":"` + data.DefaultSystemID + `"}`

func TestCreateAndFetchAnEncounterReturnsIDAndRevision(t *testing.T) {
	r, _ := encRouter(t, "5")

	w := callAs(r, "user-ok", "POST", "/encounters", basicEncounter)
	created := decodeBody(t, w)
	if w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	got := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters/"+created["id"].(string), ""))

	if w.Code != 201 || created["id"] == "" || created["revision"] != float64(1) || w.Header().Get("ETag") != `"r1"` {
		t.Fatalf("create = %d %s ETag %q", w.Code, w.Body, w.Header().Get("ETag"))
	}
	if got["id"] != created["id"] || got["name"] != "Ambush" || got["status"] != "draft" || got["combatant_count"] != float64(0) {
		t.Errorf("get = %v", got)
	}
}

func TestACallerSuppliedSessionIDIsIgnoredOnCreate(t *testing.T) {
	r, _ := encRouter(t, "5")

	created := newEncounter(t, r, "user-ok", `{"name":"X","game_system_id":"`+data.DefaultSystemID+`","session_id":"`+foreignSession+`"}`)

	if _, has := created["session_id"]; has {
		t.Fatalf("created encounter = %v, want a private encounter", created)
	}
}

func TestTurnActionReturnsTheUpdatedEncounter(t *testing.T) {
	r, _ := encRouter(t, "5")
	enc := newEncounter(t, r, "user-ok", `{"name":"Fight","game_system_id":"`+data.DefaultSystemID+`","combatants":[
		{"name":"Aria","kind":"pc","initiative":15,"health":{"hp":9,"max_hp":10}},{"name":"Orc","kind":"adversary","initiative":8,"health":{"hp":10,"max_hp":10}}]}`)
	id := enc["id"].(string)

	start := decodeBody(t, callAs(r, "user-ok", "POST", "/encounters/"+id+"/start?revision=1", ""))
	adv := callAs(r, "user-ok", "POST", "/encounters/"+id+"/advance", `{"revision":`+strconv.Itoa(revision(start))+`}`)
	advBody := decodeBody(t, adv)

	if start["status"] != "active" || start["round"] != float64(1) {
		t.Fatalf("start = %v", start)
	}
	queue := advBody["turn_queue"].([]any)
	if adv.Code != 200 || len(queue) != 2 || advBody["current_combatant_id"] != queue[1] || advBody["round"] != float64(1) {
		t.Fatalf("advance = %d %s, want the second combatant current in round 1", adv.Code, adv.Body)
	}
}

func TestTurnActionsAndResetFollowTheTrackingRules(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", `{"name":"F","game_system_id":"`+data.DefaultSystemID+`","combatants":[{"name":"A","kind":"pc","initiative":1,"health":{"hp":3,"max_hp":10}}]}`)["id"].(string)

	advBeforeStart := callAs(r, "user-ok", "POST", "/encounters/"+id+"/advance?revision=1", "")
	start := callAs(r, "user-ok", "POST", "/encounters/"+id+"/start", `{"revision":1}`)
	reset := callAs(r, "user-ok", "POST", "/encounters/"+id+"/reset?revision=2", "")
	end := callAs(r, "user-ok", "POST", "/encounters/"+id+"/end?revision=3", "")

	rb := decodeBody(t, reset)
	if advBeforeStart.Code != http.StatusUnprocessableEntity || start.Code != 200 {
		t.Fatalf("advance on a draft = %d, start = %d", advBeforeStart.Code, start.Code)
	}
	if reset.Code != 200 || rb["status"] != "draft" || rb["round"] != float64(0) || rb["combatants"].([]any)[0].(map[string]any)["health"].(map[string]any)["hp"] != float64(10) {
		t.Errorf("reset = %d %s, want a draft at round 0 with default health", reset.Code, reset.Body)
	}
	if end.Code != http.StatusUnprocessableEntity { // a draft cannot end
		t.Errorf("end on a draft = %d, want 422", end.Code)
	}
}

func TestStaleRevisionIs409WithTheCurrentEncounterAndOneConcurrentWriteWins(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", basicEncounter)["id"].(string)

	first := callAs(r, "user-ok", "PATCH", "/encounters/"+id, `{"revision":1,"name":"First"}`)
	second := callAs(r, "user-ok", "PATCH", "/encounters/"+id, `{"revision":1,"name":"Second"}`)

	body := decodeBody(t, second)
	cur, _ := body["current"].(map[string]any)
	if first.Code != 200 || second.Code != http.StatusConflict || body["code"] != "revision_conflict" || cur["name"] != "First" || cur["combatant_count"] == nil {
		t.Fatalf("first = %d, second = %d %s; want one winner and a 409 carrying the current encounter view", first.Code, second.Code, second.Body)
	}
}

func TestWritesWithoutARevisionAreRefusedAndIfMatchWorks(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", basicEncounter)["id"].(string)

	none := callAs(r, "user-ok", "PATCH", "/encounters/"+id, `{"name":"X"}`)
	bad := headerCall(r, "user-ok", "PATCH", "/encounters/"+id, `{"name":"X"}`, `nonsense`)
	ok := headerCall(r, "user-ok", "PATCH", "/encounters/"+id, `{"name":"Via header"}`, `"r1"`)

	if none.Code != 400 || bad.Code != 400 || ok.Code != 200 || decodeBody(t, ok)["name"] != "Via header" {
		t.Fatalf("no revision = %d, bad If-Match = %d, good If-Match = %d %s", none.Code, bad.Code, ok.Code, ok.Body)
	}
}

func TestHealthOutOfRangeIs422WithTheOffendingKey(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", `{"name":"F","game_system_id":"`+data.DefaultSystemID+`","combatants":[{"name":"A","kind":"pc","health":{"hp":3,"max_hp":10}}]}`)["id"].(string)
	enc := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters/"+id, ""))
	cid := enc["combatants"].([]any)[0].(map[string]any)["id"].(string)

	over := callAs(r, "user-ok", "PATCH", "/encounters/"+id+"/combatants/"+cid, `{"revision":1,"health":{"hp":11}}`)
	unknown := callAs(r, "user-ok", "PATCH", "/encounters/"+id+"/combatants/"+cid, `{"revision":1,"health":{"mana":1}}`)

	if b := decodeBody(t, over); over.Code != 422 || b["code"] != "field_out_of_range" || b["key"] != "hp" {
		t.Errorf("over max = %d %s, want 422 field_out_of_range with key hp", over.Code, over.Body)
	}
	if b := decodeBody(t, unknown); unknown.Code != 422 || b["code"] != "unknown_field" || b["key"] != "mana" {
		t.Errorf("unknown field = %d %s, want 422 unknown_field with key mana", unknown.Code, unknown.Body)
	}
}

func TestCombatantLifecycleAndVisibilitySettings(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", basicEncounter)["id"].(string)

	add := callAs(r, "user-ok", "POST", "/encounters/"+id+"/combatants", `{"revision":1,"name":"Vex","kind":"adversary","hidden":true,"health_visibility":"status","notes":"plan","initiative":12,"health":{"hp":5,"max_hp":10}}`)
	cid := decodeBody(t, add)["combatants"].([]any)[0].(map[string]any)["id"].(string)
	upd := callAs(r, "user-ok", "PATCH", "/encounters/"+id+"/combatants/"+cid, `{"revision":2,"notes_shared":true,"conditions":[{"name":"prone","rounds_remaining":2}],"health_visibility":"full"}`)
	badVis := callAs(r, "user-ok", "PATCH", "/encounters/"+id+"/combatants/"+cid, `{"revision":3,"health_visibility":"loud"}`)
	rem := callAs(r, "user-ok", "DELETE", "/encounters/"+id+"/combatants/"+cid+"?revision=3", "")
	res := callAs(r, "user-ok", "POST", "/encounters/"+id+"/combatants/"+cid+"/restore?revision=4", "")

	cb := decodeBody(t, upd)["combatants"].([]any)[0].(map[string]any)
	if add.Code != 201 || len(cid) != 24 || cb["hidden"] != true || cb["notes_shared"] != true || cb["health_visibility"] != "full" || len(cb["conditions"].([]any)) != 1 {
		t.Fatalf("add = %d, update = %d %s", add.Code, upd.Code, upd.Body)
	}
	if badVis.Code != 422 {
		t.Errorf("bad health_visibility = %d, want 422", badVis.Code)
	}
	if rem.Code != 200 || decodeBody(t, rem)["combatant_count"] != float64(0) || res.Code != 200 || decodeBody(t, res)["combatant_count"] != float64(1) {
		t.Errorf("remove = %d %s, restore = %d %s", rem.Code, rem.Body, res.Code, res.Body)
	}
}

func TestFilteredEncounterListAppliesStatusAndPageSize(t *testing.T) {
	r, _ := encRouter(t, "9")
	a := newEncounter(t, r, "user-ok", `{"name":"A","game_system_id":"`+data.DefaultSystemID+`","combatants":[{"name":"x","kind":"pc","initiative":1}]}`)
	newEncounter(t, r, "user-ok", `{"name":"B","game_system_id":"`+data.DefaultSystemID+`"}`)
	newEncounter(t, r, "user-ok", `{"name":"C","game_system_id":"`+data.DefaultSystemID+`"}`)
	callAs(r, "user-ok", "POST", "/encounters/"+a["id"].(string)+"/start?revision=1", "")

	active := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters?filter[status]=active", ""))
	page := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters?filter[status]=draft&sort=name&limit=1", ""))

	if items := active["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "A" {
		t.Errorf("active = %v", active)
	}
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "B" || page["next_cursor"] == "" {
		t.Errorf("draft page 1 = %v, want B and a cursor", page)
	}
}

func TestEncounterDeleteIsRevisionedAndOwnerScoped(t *testing.T) {
	r, _ := encRouter(t, "5")
	id := newEncounter(t, r, "user-ok", basicEncounter)["id"].(string)

	noRev := callAs(r, "user-ok", "DELETE", "/encounters/"+id, "")
	stranger := callAs(r, "user-two", "DELETE", "/encounters/"+id+"?revision=1", "")
	other := callAs(r, "user-two", "GET", "/encounters/"+id, "")
	ok := callAs(r, "user-ok", "DELETE", "/encounters/"+id+"?revision=1", "")
	gone := callAs(r, "user-ok", "GET", "/encounters/"+id, "")

	if noRev.Code != 400 || stranger.Code != 403 || other.Code != 403 || ok.Code != 204 || gone.Code != 404 {
		t.Fatalf("no revision %d, stranger delete %d, stranger get %d, owner delete %d, then get %d", noRev.Code, stranger.Code, other.Code, ok.Code, gone.Code)
	}
}

func TestSavedEncounterLimitIsTheTypedEntitlementError(t *testing.T) {
	r, _ := encRouter(t, "1")
	newEncounter(t, r, "user-ok", basicEncounter)

	w := callAs(r, "user-ok", "POST", "/encounters", basicEncounter)

	if b := decodeBody(t, w); w.Code != 403 || b["code"] != "entitlement_limit_reached" || b["key"] != "initiative.saved_encounters" {
		t.Fatalf("create at the limit = %d %s", w.Code, w.Body)
	}
}

func TestRosterTransfersReassignRandomCombatantIDsAndCountAgainstLimits(t *testing.T) {
	r, _ := encRouter(t, "2")
	roster := newRoster(t, r, "user-ok", `{"name":"Party","characters":[{"name":"Aria","kind":"pc","health":{"hp":9,"max_hp":10}},{"name":"Bo","kind":"pc"}]}`)
	rid := roster["id"].(string)

	from := callAs(r, "user-ok", "POST", "/rosters/"+rid+"/encounters", `{"name":"Tonight","game_system_id":"`+data.DefaultSystemID+`"}`)
	enc := decodeBody(t, from)
	save := callAs(r, "user-ok", "POST", "/encounters/"+enc["id"].(string)+"/save-as-roster", `{"name":"Copy"}`)
	over := callAs(r, "user-ok", "POST", "/encounters/"+enc["id"].(string)+"/save-as-roster", `{"name":"Third"}`)

	cs := enc["combatants"].([]any)
	if from.Code != 201 || len(cs) != 2 {
		t.Fatalf("from roster = %d %s", from.Code, from.Body)
	}
	for _, c := range cs {
		src := c.(map[string]any)["id"].(string)
		for _, ch := range roster["characters"].([]any) {
			if ch.(map[string]any)["id"] == src {
				t.Errorf("combatant reused the character id %s", src)
			}
		}
	}
	if save.Code != 201 || over.Code != 403 || decodeBody(t, over)["key"] != "initiative.saved_rosters" {
		t.Errorf("save-as-roster = %d, second = %d %s; want the second refused at the roster limit", save.Code, over.Code, over.Body)
	}
	if bad := callAs(r, "user-ok", "POST", "/rosters/"+rid+"/encounters", `{"name":"S","game_system_id":"`+data.DefaultSystemID+`","session_id":"`+foreignSession+`"}`); bad.Code != 201 || strings.Contains(bad.Body.String(), "session_id") {
		t.Errorf("session_id in a from-roster body = %d %s, want it ignored", bad.Code, bad.Body)
	}
}

func TestOwnerPathWritesCannotTouchASessionEncounter(t *testing.T) {
	r, db, sid := viewTable(t)
	seedSystem(t, db)
	base := "/encounters/5f00000000000000000000e1"

	for name, w := range map[string]*httptest.ResponseRecorder{
		"get":     callAs(r, "user-ok", "GET", base, ""),
		"patch":   callAs(r, "user-ok", "PATCH", base, `{"revision":3,"name":"hijack"}`),
		"delete":  callAs(r, "user-ok", "DELETE", base+"?revision=3", ""),
		"start":   callAs(r, "user-ok", "POST", base+"/start?revision=3", ""),
		"add":     callAs(r, "user-ok", "POST", base+"/combatants", `{"revision":3,"name":"x","kind":"pc"}`),
		"update":  callAs(r, "user-ok", "PATCH", base+"/combatants/hid", `{"revision":3,"notes":"x"}`),
		"save-as": callAs(r, "user-ok", "POST", base+"/save-as-roster", `{"name":"leak"}`),
	} {
		// Every refusal is 403 except save-as-roster, which has always answered 404 for an encounter
		// the owner path cannot see; either way no data comes back.
		refused := w.Code == http.StatusForbidden || (name == "save-as" && w.Code == http.StatusNotFound)
		if !refused || strings.Contains(w.Body.String(), secretName) || strings.Contains(w.Body.String(), secretNote) {
			t.Errorf("owner %s of a session encounter = %d %s, want 403 and no data", name, w.Code, w.Body)
		}
	}
	if got := decodeBody(t, callAs(r, "user-ok", "GET", encPath(sid, "5f00000000000000000000e1"), "")); got["name"] != "Ambush" || got["revision"] != float64(3) {
		t.Errorf("session encounter changed through the owner path: %v", got)
	}
}

func TestFromRosterCannotBeUsedToReachAnotherUsersRoster(t *testing.T) {
	r, _ := encRouter(t, "5")
	rid := newRoster(t, r, "user-ok", `{"name":"Mine","characters":[{"name":"Secret","kind":"pc"}]}`)["id"].(string)

	w := callAs(r, "user-two", "POST", "/rosters/"+rid+"/encounters", `{"name":"Steal","game_system_id":"`+data.DefaultSystemID+`"}`)

	if w.Code == 201 || strings.Contains(w.Body.String(), "Secret") {
		t.Fatalf("another user's from-roster = %d %s", w.Code, w.Body)
	}
}

func TestEveryEncounterRouteAnswersThroughTheViewBuilder(t *testing.T) {
	r, db, sid := viewTableLimit(t, "50")
	seedSystem(t, db)
	priv := newEncounter(t, r, "user-ok", `{"name":"P","game_system_id":"`+data.DefaultSystemID+`","combatants":[{"name":"A","kind":"pc","initiative":3,"hidden":true,"notes":"n"}]}`)
	pid, cid := priv["id"].(string), priv["combatants"].([]any)[0].(map[string]any)["id"].(string)
	roster := newRoster(t, r, "user-ok", `{"name":"R","characters":[{"name":"C","kind":"pc"}]}`)["id"].(string)
	sess := "/sessions/" + sid + "/encounters/5f00000000000000000000e1"
	rev := func() string {
		return strconv.Itoa(int(decodeBody(t, callAs(r, "user-ok", "GET", "/encounters/"+pid, ""))["revision"].(float64)))
	}

	// One entry per registered route that mentions encounters: the request to make, whether the
	// response is an encounter (or a list of them), and the token to use.
	type call struct {
		method, path, body, token string
		encounter                 bool
	}
	calls := map[string]func() call{
		"GET /encounters": func() call { return call{"GET", "/encounters", "", "user-ok", true} },
		"POST /encounters": func() call {
			return call{"POST", "/encounters", `{"name":"N","game_system_id":"` + data.DefaultSystemID + `"}`, "user-ok", true}
		},
		"GET /encounters/:id": func() call { return call{"GET", "/encounters/" + pid, "", "user-ok", true} },
		"PATCH /encounters/:id": func() call {
			return call{"PATCH", "/encounters/" + pid + "?revision=" + rev(), `{"name":"P2"}`, "user-ok", true}
		},
		"POST /encounters/:id/combatants": func() call {
			return call{"POST", "/encounters/" + pid + "/combatants?revision=" + rev(), `{"name":"B","kind":"pc"}`, "user-ok", true}
		},
		"PATCH /encounters/:id/combatants/:cid": func() call {
			return call{"PATCH", "/encounters/" + pid + "/combatants/" + cid + "?revision=" + rev(), `{"notes":"z"}`, "user-ok", true}
		},
		"DELETE /encounters/:id/combatants/:cid": func() call {
			return call{"DELETE", "/encounters/" + pid + "/combatants/" + cid + "?revision=" + rev(), "", "user-ok", true}
		},
		"POST /encounters/:id/combatants/:cid/restore": func() call {
			return call{"POST", "/encounters/" + pid + "/combatants/" + cid + "/restore?revision=" + rev(), "", "user-ok", true}
		},
		"POST /encounters/:id/start": func() call {
			return call{"POST", "/encounters/" + pid + "/start?revision=" + rev(), "", "user-ok", true}
		},
		"POST /encounters/:id/advance": func() call {
			return call{"POST", "/encounters/" + pid + "/advance?revision=" + rev(), "", "user-ok", true}
		},
		"POST /encounters/:id/back": func() call {
			return call{"POST", "/encounters/" + pid + "/back?revision=" + rev(), "", "user-ok", true}
		},
		"POST /encounters/:id/end": func() call { return call{"POST", "/encounters/" + pid + "/end?revision=" + rev(), "", "user-ok", true} },
		"POST /encounters/:id/reset": func() call {
			return call{"POST", "/encounters/" + pid + "/reset?revision=" + rev(), "", "user-ok", true}
		},
		"POST /rosters/:id/encounters": func() call {
			return call{"POST", "/rosters/" + roster + "/encounters", `{"name":"F","game_system_id":"` + data.DefaultSystemID + `"}`, "user-ok", true}
		},
		"POST /encounters/:id/save-as-roster": func() call {
			return call{"POST", "/encounters/" + pid + "/save-as-roster", `{"name":"S"}`, "user-ok", false}
		},
		"DELETE /encounters/:id":                            func() call { return call{"DELETE", "/encounters/" + pid + "?revision=" + rev(), "", "user-ok", false} },
		"GET /sessions/:id/encounters":                      func() call { return call{"GET", "/sessions/" + sid + "/encounters", "", "user-two", true} },
		"GET /sessions/:id/encounters/:eid":                 func() call { return call{"GET", sess, "", "user-two", true} },
		"GET /sessions/:id/encounters/:eid/combatants/:cid": func() call { return call{"GET", sess + "/combatants/pc", "", "user-two", true} },
	}
	order := []string{"GET /encounters", "POST /encounters", "GET /encounters/:id", "PATCH /encounters/:id", "POST /encounters/:id/combatants",
		"PATCH /encounters/:id/combatants/:cid", "DELETE /encounters/:id/combatants/:cid", "POST /encounters/:id/combatants/:cid/restore",
		"POST /encounters/:id/start", "POST /encounters/:id/advance", "POST /encounters/:id/back", "POST /encounters/:id/end", "POST /encounters/:id/reset",
		"POST /rosters/:id/encounters", "POST /encounters/:id/save-as-roster", "GET /sessions/:id/encounters", "GET /sessions/:id/encounters/:eid",
		"GET /sessions/:id/encounters/:eid/combatants/:cid", "DELETE /encounters/:id"}

	for _, route := range r.(*gin.Engine).Routes() {
		if !strings.Contains(route.Path, "encounters") {
			continue
		}
		if _, ok := calls[route.Method+" "+route.Path]; !ok {
			t.Errorf("route %s %s has no entry in this test: add one, and make sure its response is built by the view builder", route.Method, route.Path)
		}
	}
	for _, name := range order {
		c := calls[name]()
		w := callAs(r, c.token, c.method, c.path, c.body)
		if w.Code >= 300 {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
			continue
		}
		if !c.encounter {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		items := []any{body}
		if list, ok := body["items"].([]any); ok {
			items = list
		}
		for _, it := range items {
			m, _ := it.(map[string]any)
			// combatant_count exists only in a built view; a serialized data.Encounter or Combatant lacks it.
			if m == nil || (m["combatant_count"] == nil && m["combatants"] != nil) || (m["combatant_count"] == nil && m["kind"] == nil) {
				t.Errorf("%s returned %v, which the view builder did not build", name, m)
			}
		}
	}
}

func TestIntegrationCredentialsFollowScopeByMethodAndNeverReachSessionRoutes(t *testing.T) {
	r, _, sid := viewTableLimit(t, "50")
	_, readOnly := issue(t, r, "user-ok", `["read"]`)
	_, writeOnly := issue(t, r, "user-ok", `["write"]`)
	_, both := issue(t, r, "user-ok", `["read","write"]`)
	sub := strings.NewReplacer(":id", sid, ":eid", "5f00000000000000000000e1", ":cid", "x", ":invite", "x", ":request", "x", ":user", "u-2")

	checked, session := 0, 0
	for _, route := range r.(*gin.Engine).Routes() {
		if strings.HasPrefix(route.Path, "/admin") || strings.HasPrefix(route.Path, "/status") || route.Path == "/probe" {
			continue
		}
		path := sub.Replace(route.Path)
		read := route.Method == http.MethodGet || route.Method == http.MethodHead
		isSession := strings.HasPrefix(route.Path, "/sessions")
		checked++
		for name, token := range map[string]string{"read-only": readOnly, "write-only": writeOnly, "read+write": both} {
			w := callAs(r, token, route.Method, path, `{}`)
			code := decodeBody(t, w)["code"]
			switch {
			case isSession || strings.HasPrefix(route.Path, "/credentials") || strings.HasPrefix(route.Path, "/entitlements") || strings.HasPrefix(route.Path, "/themes"):
				session++
				// A credential without the method's scope is stopped by the scope check first; one with
				// it (read+write) by userOnly. Either way the route is closed to credentials.
				if w.Code != http.StatusForbidden || (name == "read+write" && code != "user_token_required") {
					t.Errorf("%s credential %s %s = %d %v, want 403 (account and session routes need the user's own token)", name, route.Method, route.Path, w.Code, code)
				}
			case (read && name == "write-only") || (!read && name == "read-only"):
				if w.Code != http.StatusForbidden || code != "scope_missing" {
					t.Errorf("%s credential %s %s = %d %v, want 403 scope_missing (scope follows the method)", name, route.Method, route.Path, w.Code, code)
				}
			default:
				if code == "scope_missing" || code == "user_token_required" {
					t.Errorf("%s credential %s %s was refused for scope: %d %v", name, route.Method, route.Path, w.Code, code)
				}
			}
		}
	}
	if checked < 30 || session < 10 {
		t.Fatalf("walked %d routes, %d of them account or session routes; the walk is not covering the router", checked, session)
	}
}
