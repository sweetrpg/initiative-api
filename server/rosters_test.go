package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newRoster(t *testing.T, r http.Handler, token, body string) map[string]any {
	t.Helper()
	w := callAs(r, token, "POST", "/rosters", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create roster = %d %s", w.Code, w.Body)
	}
	return decodeBody(t, w)
}

func TestRosterCreateGetListAndOwnerScoping(t *testing.T) {
	r := credRouter(t, "5")
	created := newRoster(t, r, "user-ok", `{"name":"Party","favorite":true,"characters":[{"name":"Aria","kind":"pc","health":{"hp":9}}]}`)
	id := created["id"].(string)

	got := callAs(r, "user-ok", "GET", "/rosters/"+id, "")
	list := callAs(r, "user-ok", "GET", "/rosters", "")
	other := callAs(r, "user-two", "GET", "/rosters/"+id, "")
	otherList := callAs(r, "user-two", "GET", "/rosters", "")

	if got.Code != 200 || decodeBody(t, got)["name"] != "Party" || created["revision"] != float64(1) {
		t.Fatalf("get = %d %s", got.Code, got.Body)
	}
	if len(decodeBody(t, list)["items"].([]any)) != 1 || len(decodeBody(t, otherList)["items"].([]any)) != 0 {
		t.Errorf("list = %s / other user's list = %s", list.Body, otherList.Body)
	}
	if other.Code == 200 || strings.Contains(other.Body.String(), "Party") {
		t.Errorf("another user read the roster: %d %s", other.Code, other.Body)
	}
}

func TestRosterWritesNeedARevisionAndStaleOnesConflictWithTheCurrentRoster(t *testing.T) {
	r := credRouter(t, "5")
	created := newRoster(t, r, "user-ok", `{"name":"Party"}`)
	id := created["id"].(string)

	missing := callAs(r, "user-ok", "PATCH", "/rosters/"+id, `{"name":"X"}`)
	ok := callAs(r, "user-ok", "PATCH", "/rosters/"+id, `{"revision":1,"name":"Crew"}`)
	stale := callAs(r, "user-ok", "PATCH", "/rosters/"+id, `{"revision":1,"name":"Stale"}`)
	viaHeader := headerCall(r, "user-ok", "PATCH", "/rosters/"+id, `{"favorite":true}`, `"r2"`)

	if missing.Code != http.StatusBadRequest {
		t.Errorf("write without a revision = %d, want 400", missing.Code)
	}
	if ok.Code != 200 || decodeBody(t, ok)["name"] != "Crew" || decodeBody(t, ok)["revision"] != float64(2) {
		t.Errorf("patch = %d %s", ok.Code, ok.Body)
	}
	body := decodeBody(t, stale)
	if stale.Code != http.StatusConflict || body["code"] != "revision_conflict" || body["current"].(map[string]any)["name"] != "Crew" {
		t.Errorf("stale patch = %d %s, want 409 revision_conflict carrying the current roster", stale.Code, stale.Body)
	}
	if viaHeader.Code != 200 || decodeBody(t, viaHeader)["favorite"] != true || decodeBody(t, viaHeader)["name"] != "Crew" {
		t.Errorf("patch via If-Match = %d %s, want name kept and favorite set", viaHeader.Code, viaHeader.Body)
	}
}

func TestRosterCharactersAddReplaceRemove(t *testing.T) {
	r := credRouter(t, "5")
	id := newRoster(t, r, "user-ok", `{"name":"Party"}`)["id"].(string)

	add := callAs(r, "user-ok", "POST", "/rosters/"+id+"/characters", `{"revision":1,"name":"Orc","kind":"adversary"}`)
	charID := decodeBody(t, add)["characters"].([]any)[0].(map[string]any)["id"].(string)
	replace := callAs(r, "user-ok", "PUT", "/rosters/"+id+"/characters/"+charID, `{"revision":2,"name":"Orc Chief","kind":"adversary","notes":"big"}`)
	bad := callAs(r, "user-ok", "POST", "/rosters/"+id+"/characters", `{"revision":3,"name":"Ghost","kind":"spirit"}`)
	remove := callAs(r, "user-ok", "DELETE", "/rosters/"+id+"/characters/"+charID+"?revision=3", "")

	if add.Code != 201 || replace.Code != 200 || !strings.Contains(replace.Body.String(), "Orc Chief") {
		t.Fatalf("add = %d, replace = %d %s", add.Code, replace.Code, replace.Body)
	}
	if bad.Code != http.StatusUnprocessableEntity || decodeBody(t, bad)["code"] != "invalid" {
		t.Errorf("bad kind = %d %s, want 422 invalid", bad.Code, bad.Body)
	}
	if remove.Code != 200 || len(decodeBody(t, remove)["characters"].([]any)) != 0 {
		t.Errorf("remove = %d %s", remove.Code, remove.Body)
	}
}

func TestRosterDeleteIsRevisionedAndOwnerScoped(t *testing.T) {
	r := credRouter(t, "5")
	id := newRoster(t, r, "user-ok", `{"name":"Party"}`)["id"].(string)

	noRev := callAs(r, "user-ok", "DELETE", "/rosters/"+id, "")
	stranger := callAs(r, "user-two", "DELETE", "/rosters/"+id+"?revision=1", "")
	ok := callAs(r, "user-ok", "DELETE", "/rosters/"+id+"?revision=1", "")
	gone := callAs(r, "user-ok", "GET", "/rosters/"+id, "")

	if noRev.Code != 400 || stranger.Code == 204 || ok.Code != 204 || gone.Code != 404 {
		t.Fatalf("delete without revision = %d, by a stranger = %d, by the owner = %d, then get = %d", noRev.Code, stranger.Code, ok.Code, gone.Code)
	}
}

func TestRosterCreateAtTheLimitIsTheTypedEntitlementError(t *testing.T) {
	r := credRouter(t, "2")
	newRoster(t, r, "user-ok", `{"name":"A"}`)
	newRoster(t, r, "user-ok", `{"name":"B"}`)

	w := callAs(r, "user-ok", "POST", "/rosters", `{"name":"C"}`)

	body := decodeBody(t, w)
	if w.Code != http.StatusForbidden || body["code"] != "entitlement_limit_reached" || body["key"] != "initiative.saved_rosters" || body["limit"] != float64(2) {
		t.Fatalf("create at the limit = %d %s, want 403 entitlement_limit_reached with the key and limit", w.Code, w.Body)
	}
}

func TestRosterListConventions(t *testing.T) {
	r := credRouter(t, "9")
	for _, b := range []string{`{"name":"Bravo","favorite":true}`, `{"name":"Alpha"}`, `{"name":"Charlie","favorite":true}`} {
		newRoster(t, r, "user-ok", b)
	}
	names := func(path string) []string {
		w := callAs(r, "user-ok", "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
		var out []string
		for _, it := range decodeBody(t, w)["items"].([]any) {
			out = append(out, it.(map[string]any)["name"].(string))
		}
		return out
	}

	if got := names("/rosters?sort=name"); strings.Join(got, ",") != "Alpha,Bravo,Charlie" {
		t.Errorf("sort=name = %v", got)
	}
	if got := names("/rosters?sort=-name"); strings.Join(got, ",") != "Charlie,Bravo,Alpha" {
		t.Errorf("sort=-name = %v", got)
	}
	if got := names("/rosters?sort=name&filter[favorite]=true"); strings.Join(got, ",") != "Bravo,Charlie" {
		t.Errorf("filter favorite = %v", got)
	}
	if got := names("/rosters?filter[name][in]=Alpha,Charlie&sort=name"); strings.Join(got, ",") != "Alpha,Charlie" {
		t.Errorf("filter in = %v", got)
	}
	if got := names("/rosters?filter[name][contains]=rav"); strings.Join(got, ",") != "Bravo" {
		t.Errorf("filter contains = %v", got)
	}
	w := callAs(r, "user-ok", "GET", "/rosters?sort=name&limit=2", "")
	first := decodeBody(t, w)
	if len(first["items"].([]any)) != 2 || first["next_cursor"] == "" {
		t.Fatalf("page 1 = %s, want 2 items and a cursor", w.Body)
	}
	second := decodeBody(t, callAs(r, "user-ok", "GET", "/rosters?sort=name&limit=2&cursor="+first["next_cursor"].(string), ""))
	if len(second["items"].([]any)) != 1 || second["items"].([]any)[0].(map[string]any)["name"] != "Charlie" || second["next_cursor"] != "" {
		t.Errorf("page 2 = %v", second)
	}
}

func TestRosterListRejectsWhatTheSpecDoesNotAllow(t *testing.T) {
	r := credRouter(t, "5")
	for path, want := range map[string]int{
		"/rosters?limit=0":                     400,
		"/rosters?limit=x":                     400,
		"/rosters?filter[favorite]=maybe":      400,
		"/rosters?filter[owner_user_id]=u-2":   422,
		"/rosters?sort=owner_user_id":          422,
		"/rosters?fields=owner_user_id,secret": 422,
		"/rosters?filter[name][contains]=":     422,
		"/rosters?filter[name][regex]=.*":      200, // not a filter key: ignored, never an operator
	} {
		if w := callAs(r, "user-ok", "GET", path, ""); w.Code != want {
			t.Errorf("GET %s = %d %s, want %d", path, w.Code, w.Body, want)
		}
	}
}

func TestIntegrationCredentialScopesApplyToRosters(t *testing.T) {
	r := credRouter(t, "5")
	newRoster(t, r, "user-ok", `{"name":"Party"}`)
	_, readOnly := issue(t, r, "user-ok", `["read"]`)

	read := callAs(r, readOnly, "GET", "/rosters", "")
	write := callAs(r, readOnly, "POST", "/rosters", `{"name":"Nope"}`)

	if read.Code != 200 || len(decodeBody(t, read)["items"].([]any)) != 1 {
		t.Errorf("read-only credential GET = %d %s, want the owner's roster", read.Code, read.Body)
	}
	if write.Code != http.StatusForbidden || decodeBody(t, write)["code"] != "scope_missing" {
		t.Errorf("read-only credential POST = %d %s, want 403 scope_missing", write.Code, write.Body)
	}
}

// headerCall is callAs with an If-Match header.
func headerCall(r http.Handler, token, method, path, body, ifMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
