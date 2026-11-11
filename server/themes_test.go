package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// themeRouter serves the real handlers over a fake database. slots is the entitled slot limit;
// entStatus is what the fake entitlements-api answers (200 serves the limit).
func themeRouter(t *testing.T, slots, entStatus int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entStatus != http.StatusOK {
			w.WriteHeader(entStatus)
			return
		}
		_, _ = w.Write([]byte(`{"key":"initiative.theme_slots","type":"limit","value":` + strconv.Itoa(slots) + `,"ttl_seconds":0}`))
	}))
	t.Cleanup(ent.Close)
	db := fake.NewDB()
	themes, err := domain.NewThemes(db)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(Correlation())
	SetupHandlers(r, Services{
		Authz:        authz.NewClient(fakeAuthAPI(t).URL, fakeUsersAPI(t).URL),
		Entitlements: entitlements.New(ent.URL),
		Themes:       themes,
		Rosters:      domain.NewRosters(db),
		Encounters:   domain.NewEncounters(db),
	})
	return r
}

func call(r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer user-ok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func theme(name string) map[string]string {
	return map[string]string{
		"name": name, "style": "dark", "main_color": "#FFFFFF", "accent1_color": "#AA0000",
		"accent2_color": "#00AA00", "background1_color": "#000000", "background2_color": "#111111",
		"primary_font_name": "Cinzel", "secondary_font_name": "Lato",
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestThemeRoutesRequireAuth(t *testing.T) {
	r := themeRouter(t, 3, 200)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/themes", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestThemeCreateGetListUpdateDelete(t *testing.T) {
	r := themeRouter(t, 3, 200)
	w := call(r, "POST", "/themes", theme("Dungeon"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	created := decodeBody(t, w)
	id, rev := created["id"].(string), int64(created["revision"].(float64))
	if created["slot"].(float64) != 0 {
		t.Fatalf("slot = %v, want 0", created["slot"])
	}
	if w := call(r, "GET", "/themes/"+id, nil); w.Code != 200 {
		t.Fatalf("get = %d", w.Code)
	}
	list := decodeBody(t, call(r, "GET", "/themes", nil))
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("list = %v", list)
	}
	w = call(r, "PATCH", "/themes/"+id, map[string]any{"revision": rev, "name": "Crypt"})
	if w.Code != 200 || decodeBody(t, w)["name"] != "Crypt" {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := call(r, "DELETE", "/themes/"+id+"?revision="+strconv.FormatInt(rev+1, 10), nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if w := call(r, "GET", "/themes/"+id, nil); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

func TestThemeCreateOverLimitReturnsTypedEntitlementError(t *testing.T) {
	r := themeRouter(t, 1, 200)
	call(r, "POST", "/themes", theme("A"))
	w := call(r, "POST", "/themes", theme("B"))
	body := decodeBody(t, w)
	if w.Code != http.StatusForbidden || body["code"] != "entitlement_limit_reached" ||
		body["key"] != "initiative.theme_slots" || body["limit"] != float64(1) || body["correlation_id"] == "" {
		t.Fatalf("got %d %v", w.Code, body)
	}
}

func TestThemeCreateWhenEntitlementsDownIs503(t *testing.T) {
	r := themeRouter(t, 1, http.StatusBadGateway)
	w := call(r, "POST", "/themes", theme("A"))
	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "entitlements_unavailable" {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestThemeReadsAndEditsWorkWhenEntitlementsDown(t *testing.T) {
	r := themeRouter(t, 1, http.StatusBadGateway)
	if w := call(r, "GET", "/themes", nil); w.Code != 200 {
		t.Fatalf("list = %d", w.Code)
	}
	if w := call(r, "GET", "/themes/packaged", nil); w.Code != 200 {
		t.Fatalf("packaged = %d", w.Code)
	}
}

func TestPackagedThemesReadOnly(t *testing.T) {
	r := themeRouter(t, 1, 200)
	items := decodeBody(t, call(r, "GET", "/themes/packaged", nil))["items"].([]any)
	id := items[0].(map[string]any)["id"].(string)
	if w := call(r, "GET", "/themes/"+id, nil); w.Code != 200 {
		t.Fatalf("get packaged = %d", w.Code)
	}
	w := call(r, "PATCH", "/themes/"+id, map[string]any{"revision": 1, "name": "x"})
	if w.Code != http.StatusForbidden || decodeBody(t, w)["code"] != "packaged_theme_read_only" {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := call(r, "DELETE", "/themes/"+id+"?revision=1", nil); w.Code != http.StatusForbidden {
		t.Fatalf("delete = %d", w.Code)
	}
}

func TestThemeStaleRevisionReturnsCurrent(t *testing.T) {
	r := themeRouter(t, 2, 200)
	id := decodeBody(t, call(r, "POST", "/themes", theme("A")))["id"].(string)
	w := call(r, "PATCH", "/themes/"+id, map[string]any{"revision": 9, "name": "x"})
	body := decodeBody(t, w)
	if w.Code != http.StatusConflict || body["code"] != "revision_conflict" || body["current"] == nil {
		t.Fatalf("got %d %v", w.Code, body)
	}
}

func TestThemeBadInput(t *testing.T) {
	r := themeRouter(t, 2, 200)
	bad := theme("A")
	bad["main_color"] = "blue"
	if w := call(r, "POST", "/themes", bad); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad color = %d, want 422", w.Code)
	}
	if w := call(r, "PATCH", "/themes/x", map[string]any{"name": "n"}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing revision = %d, want 400", w.Code)
	}
	if w := call(r, "DELETE", "/themes/x", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing revision on delete = %d, want 400", w.Code)
	}
	if w := call(r, "GET", "/themes?limit=abc", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", w.Code)
	}
}
