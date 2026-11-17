package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
)

func TestBodiesOverTheCapAre413(t *testing.T) {
	r, _ := encRouter(t, "5")
	big := `{"name":"` + strings.Repeat("x", maxBodyBytes+10) + `"}`

	for _, path := range []string{"/rosters", "/encounters"} {
		w := callAs(r, "user-ok", "POST", path, big)
		if w.Code != http.StatusRequestEntityTooLarge || decodeBody(t, w)["code"] != "payload_too_large" {
			t.Errorf("POST %s with a %d byte body = %d %s, want 413 payload_too_large", path, len(big), w.Code, w.Body)
		}
	}
}

func TestUnknownBodyFieldsAre422AndTrailingJSONIs400(t *testing.T) {
	r, _ := encRouter(t, "5")
	rid := newRoster(t, r, "user-ok", `{"name":"Party"}`)["id"].(string)

	chars := callAs(r, "user-ok", "PATCH", "/rosters/"+rid, `{"revision":1,"characters":[{"name":"x","kind":"pc"}]}`)
	typo := callAs(r, "user-ok", "POST", "/encounters", `{"name":"x","game_system_id":"none","orderring":"highToLow"}`)
	two := callAs(r, "user-ok", "POST", "/rosters", `{"name":"a"}{"name":"b"}`)

	if b := decodeBody(t, chars); chars.Code != 422 || b["code"] != "unknown_body_field" || b["key"] != "characters" {
		t.Errorf("characters in a roster PATCH = %d %s, want 422 unknown_body_field", chars.Code, chars.Body)
	}
	if b := decodeBody(t, typo); typo.Code != 422 || b["key"] != "orderring" {
		t.Errorf("misspelled field = %d %s", typo.Code, typo.Body)
	}
	if two.Code != 400 {
		t.Errorf("two JSON values = %d, want 400", two.Code)
	}
}

func TestFieldsProjectTheRenderedResourceWithoutZeroFilledValues(t *testing.T) {
	r, _ := encRouter(t, "5")
	roster := newRoster(t, r, "user-ok", `{"name":"Party","favorite":true,"characters":[{"name":"A","kind":"pc"}]}`)
	enc := newEncounter(t, r, "user-ok", `{"name":"Fight","game_system_id":"none","combatants":[{"name":"A","kind":"pc"}]}`)

	rl := decodeBody(t, callAs(r, "user-ok", "GET", "/rosters?fields=name", ""))["items"].([]any)[0].(map[string]any)
	rg := decodeBody(t, callAs(r, "user-ok", "GET", "/rosters/"+roster["id"].(string)+"?fields=favorite", ""))
	el := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters?fields=status", ""))["items"].([]any)[0].(map[string]any)
	eg := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters/"+enc["id"].(string)+"?fields=round,combatant_count", ""))

	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	for name, got := range map[string]struct {
		m    map[string]any
		want map[string]bool
	}{
		"roster list":    {rl, map[string]bool{"id": true, "revision": true, "name": true}},
		"roster get":     {rg, map[string]bool{"id": true, "revision": true, "favorite": true}},
		"encounter list": {el, map[string]bool{"id": true, "revision": true, "status": true}},
		"encounter get":  {eg, map[string]bool{"id": true, "revision": true, "round": true, "combatant_count": true}},
	} {
		for _, k := range keys(got.m) {
			if !got.want[k] {
				t.Errorf("%s has %q, which was not selected: %v", name, k, got.m)
			}
		}
		if len(got.m) != len(got.want) {
			t.Errorf("%s = %v, want exactly %v", name, got.m, got.want)
		}
	}
	for _, path := range []string{"/rosters?fields=owner_user_id,secret", "/encounters?fields=nope", "/encounters/" + enc["id"].(string) + "?fields=ref_keys"} {
		if w := callAs(r, "user-ok", "GET", path, ""); w.Code != 422 {
			t.Errorf("GET %s = %d, want 422 for a field that cannot be projected", path, w.Code)
		}
	}
}

func TestSizeAndNameBoundsAreTyped422(t *testing.T) {
	r, _ := encRouter(t, "50")
	long := strings.Repeat("n", data.MaxNameLen+1)
	manyCombatants := `[` + strings.TrimSuffix(strings.Repeat(`{"name":"c","kind":"pc"},`, data.MaxCombatants+1), ",") + `]`
	manyCharacters := `[` + strings.TrimSuffix(strings.Repeat(`{"name":"c","kind":"pc"},`, data.MaxCharacters+1), ",") + `]`
	manyConditions := `[` + strings.TrimSuffix(strings.Repeat(`{"name":"c"},`, data.MaxConditions+1), ",") + `]`
	manyHealth := `{` + strings.TrimSuffix(func() string {
		var b strings.Builder
		for i := 0; i <= data.MaxMapEntries; i++ {
			b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":1,`)
		}
		return b.String()
	}(), ",") + `}`
	enc := newEncounter(t, r, "user-ok", `{"name":"E","game_system_id":"none"}`)
	eid := enc["id"].(string)
	rid := newRoster(t, r, "user-ok", `{"name":"R"}`)["id"].(string)

	for name, tc := range map[string]struct{ method, path, body string }{
		"long roster name":           {"POST", "/rosters", `{"name":"` + long + `"}`},
		"long encounter name":        {"POST", "/encounters", `{"name":"` + long + `","game_system_id":"none"}`},
		"too many combatants":        {"POST", "/encounters", `{"name":"x","game_system_id":"none","combatants":` + manyCombatants + `}`},
		"too many characters":        {"POST", "/rosters", `{"name":"x","characters":` + manyCharacters + `}`},
		"too many conditions":        {"POST", "/encounters/" + eid + "/combatants?revision=1", `{"name":"x","kind":"pc","conditions":` + manyConditions + `}`},
		"too many health entries":    {"POST", "/encounters/" + eid + "/combatants?revision=1", `{"name":"x","kind":"pc","health":` + manyHealth + `}`},
		"long notes":                 {"POST", "/encounters/" + eid + "/combatants?revision=1", `{"name":"x","kind":"pc","notes":"` + strings.Repeat("z", data.MaxNotesLen+1) + `"}`},
		"blank encounter name patch": {"PATCH", "/encounters/" + eid + "?revision=1", `{"name":"   "}`},
		"long encounter name patch":  {"PATCH", "/encounters/" + eid + "?revision=1", `{"name":"` + long + `"}`},
		"negative position":          {"PATCH", "/encounters/" + eid + "?revision=1", `{"position":-1}`},
		"blank roster name patch":    {"PATCH", "/rosters/" + rid + "?revision=1", `{"name":" "}`},
		"long roster name patch":     {"PATCH", "/rosters/" + rid + "?revision=1", `{"name":"` + long + `"}`},
		"unknown theme":              {"PATCH", "/encounters/" + eid + "?revision=1", `{"theme_id":"5f0000000000000000000abc"}`},
		"long theme id":              {"PATCH", "/encounters/" + eid + "?revision=1", `{"theme_id":"` + strings.Repeat("t", data.MaxThemeIDLen+1) + `"}`},
	} {
		w := callAs(r, "user-ok", tc.method, tc.path, tc.body)
		if w.Code != http.StatusUnprocessableEntity || decodeBody(t, w)["code"] != "invalid" {
			t.Errorf("%s = %d %.200s, want 422 invalid", name, w.Code, w.Body)
		}
	}
	if w := callAs(r, "user-ok", "PATCH", "/encounters/"+eid+"?revision=1", `{"theme_id":"pkg-midnight","name":"  Trimmed  "}`); w.Code != 200 || decodeBody(t, w)["name"] != "Trimmed" {
		t.Errorf("packaged theme id and a padded name = %d %s, want accepted and trimmed", w.Code, w.Body)
	}
}

func TestAnotherUsersThemeIsNotAcceptable(t *testing.T) {
	r, _ := encRouter(t, "5")
	theme := callAs(r, "user-two", "POST", "/themes", themeJSON("Theirs"))
	if theme.Code != 201 {
		t.Skipf("theme create = %d; themes need their own router limits", theme.Code)
	}
	themeID := decodeBody(t, theme)["id"].(string)
	eid := newEncounter(t, r, "user-ok", `{"name":"E","game_system_id":"none"}`)["id"].(string)

	w := callAs(r, "user-ok", "PATCH", "/encounters/"+eid+"?revision=1", `{"theme_id":"`+themeID+`"}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("another user's theme = %d %s, want 422", w.Code, w.Body)
	}
}

func TestSaveAsRosterWithoutANameUsesTheEncountersName(t *testing.T) {
	r, _ := encRouter(t, "5")
	eid := newEncounter(t, r, "user-ok", `{"name":"Tonight","game_system_id":"none"}`)["id"].(string)

	w := callAs(r, "user-ok", "POST", "/encounters/"+eid+"/save-as-roster", "")

	if w.Code != 201 || decodeBody(t, w)["name"] != "Tonight" {
		t.Fatalf("save-as-roster with no body = %d %s, want a roster named like the encounter", w.Code, w.Body)
	}
}

func TestEveryWriteRouteRefusesAnotherUserWith403AndNoDocument(t *testing.T) {
	r, db, _ := viewTableLimit(t, "50")
	seedSystem(t, db)
	enc := newEncounter(t, r, "user-ok", `{"name":"MarkerEncounter","game_system_id":"none","combatants":[{"name":"MarkerCombatant","kind":"pc"}]}`)
	eid, cid := enc["id"].(string), enc["combatants"].([]any)[0].(map[string]any)["id"].(string)
	roster := newRoster(t, r, "user-ok", `{"name":"MarkerRoster","characters":[{"name":"MarkerCharacter","kind":"pc"}]}`)
	rid, chid := roster["id"].(string), roster["characters"].([]any)[0].(map[string]any)["id"].(string)

	calls := map[string][2]string{
		"PATCH /rosters/:id":                           {"/rosters/" + rid + "?revision=1", `{"name":"x"}`},
		"DELETE /rosters/:id":                          {"/rosters/" + rid + "?revision=1", ""},
		"POST /rosters/:id/characters":                 {"/rosters/" + rid + "/characters?revision=1", `{"name":"x","kind":"pc"}`},
		"PUT /rosters/:id/characters/:cid":             {"/rosters/" + rid + "/characters/" + chid + "?revision=1", `{"name":"x","kind":"pc"}`},
		"DELETE /rosters/:id/characters/:cid":          {"/rosters/" + rid + "/characters/" + chid + "?revision=1", ""},
		"POST /rosters/:id/encounters":                 {"/rosters/" + rid + "/encounters", `{"name":"x","game_system_id":"none"}`},
		"PATCH /encounters/:id":                        {"/encounters/" + eid + "?revision=1", `{"name":"x"}`},
		"DELETE /encounters/:id":                       {"/encounters/" + eid + "?revision=1", ""},
		"POST /encounters/:id/combatants":              {"/encounters/" + eid + "/combatants?revision=1", `{"name":"x","kind":"pc"}`},
		"PATCH /encounters/:id/combatants/:cid":        {"/encounters/" + eid + "/combatants/" + cid + "?revision=1", `{"notes":"x"}`},
		"DELETE /encounters/:id/combatants/:cid":       {"/encounters/" + eid + "/combatants/" + cid + "?revision=1", ""},
		"POST /encounters/:id/combatants/:cid/restore": {"/encounters/" + eid + "/combatants/" + cid + "/restore?revision=1", ""},
		"POST /encounters/:id/start":                   {"/encounters/" + eid + "/start?revision=1", ""},
		"POST /encounters/:id/end":                     {"/encounters/" + eid + "/end?revision=1", ""},
		"POST /encounters/:id/reset":                   {"/encounters/" + eid + "/reset?revision=1", ""},
		"POST /encounters/:id/advance":                 {"/encounters/" + eid + "/advance?revision=1", ""},
		"POST /encounters/:id/back":                    {"/encounters/" + eid + "/back?revision=1", ""},
		"POST /encounters/:id/save-as-roster":          {"/encounters/" + eid + "/save-as-roster", `{"name":"x"}`},
		"GET /encounters/:id":                          {"/encounters/" + eid, ""},
		"GET /rosters/:id":                             {"/rosters/" + rid, ""},
	}
	for _, route := range r.(*gin.Engine).Routes() {
		if route.Method == http.MethodGet && !strings.Contains(route.Path, ":id") {
			continue
		}
		if !strings.HasPrefix(route.Path, "/rosters/") && !strings.HasPrefix(route.Path, "/encounters/") {
			continue
		}
		if _, ok := calls[route.Method+" "+route.Path]; !ok {
			t.Errorf("route %s %s has no entry in the other-user table: add one", route.Method, route.Path)
		}
	}
	for name, c := range calls {
		method, _, _ := strings.Cut(name, " ")
		w := callAs(r, "user-two", method, c[0], c[1])
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "Marker") {
			t.Errorf("another user's %s = %d %s, want 403 and no document", name, w.Code, w.Body)
		}
	}
	// Nothing changed for the owner.
	got := decodeBody(t, callAs(r, "user-ok", "GET", "/encounters/"+eid, ""))
	if got["name"] != "MarkerEncounter" || got["revision"] != float64(1) {
		t.Errorf("the encounter changed: %v", got)
	}
}

func TestTrailingBytesPastTheCapAre413NotBadRequest(t *testing.T) {
	r, _ := encRouter(t, "5")
	body := `{"name":"a"}` + strings.Repeat(" ", maxBodyBytes+10)

	w := callAs(r, "user-ok", "POST", "/rosters", body)

	if w.Code != http.StatusRequestEntityTooLarge || decodeBody(t, w)["code"] != "payload_too_large" {
		t.Fatalf("valid JSON then %d bytes of padding = %d %s, want 413 payload_too_large", len(body), w.Code, w.Body)
	}
}

func TestThemeAndCredentialWritesRejectAnUnknownBodyField(t *testing.T) {
	r, _ := encRouter(t, "5")
	for _, route := range []struct{ path, body string }{
		{"/themes", `{"name":"x","surprise":1}`},
		{"/credentials", `{"label":"x","scopes":["read"],"surprise":1}`},
	} {
		w := callAs(r, "user-ok", "POST", route.path, route.body)
		if b := decodeBody(t, w); w.Code != 422 || b["code"] != "unknown_body_field" || b["key"] != "surprise" {
			t.Errorf("POST %s with an unknown field = %d %s, want 422 unknown_body_field", route.path, w.Code, w.Body)
		}
	}
}
