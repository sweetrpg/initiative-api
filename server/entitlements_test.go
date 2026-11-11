package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

const batchBody = `{"results":[
 {"key":"initiative.theme_slots","type":"limit","value":3,"ttl_seconds":0},
 {"key":"initiative.saved_rosters","type":"limit","value":5,"ttl_seconds":0},
 {"key":"initiative.sessions_per_gm","type":"limit","value":1,"ttl_seconds":0},
 {"key":"initiative.deluxe","type":"flag","value":true,"ttl_seconds":0}]}`

func entitlementsRouter(t *testing.T, entStatus int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entStatus != http.StatusOK {
			w.WriteHeader(entStatus)
			return
		}
		if r.URL.Path == "/check/batch" && r.URL.Query().Get("prefix") == "initiative." {
			_, _ = w.Write([]byte(batchBody))
			return
		}
		_, _ = w.Write([]byte(`{"key":"initiative.theme_slots","type":"limit","value":3,"ttl_seconds":0}`))
	}))
	t.Cleanup(ent.Close)
	db := fake.NewDB()
	themes, _ := domain.NewThemes(db)
	r := gin.New()
	r.Use(Correlation())
	SetupHandlers(r, Services{
		Authz:        authz.NewClient(fakeAuthAPI(t).URL, fakeUsersAPI(t).URL),
		Entitlements: entitlements.New(ent.URL),
		Themes:       themes, Rosters: domain.NewRosters(db), Encounters: domain.NewEncounters(db),
	})
	return r
}

func TestEntitlementsShowValuesAndUsage(t *testing.T) {
	r := entitlementsRouter(t, 200)
	call(r, "POST", "/themes", theme("A"))
	call(r, "POST", "/themes", theme("B"))

	w := call(r, "GET", "/entitlements", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	byKey := map[string]map[string]any{}
	for _, it := range decodeBody(t, w)["items"].([]any) {
		m := it.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if byKey["initiative.theme_slots"]["value"] != float64(3) || byKey["initiative.theme_slots"]["usage"] != float64(2) {
		t.Errorf("theme_slots = %v, want value 3 usage 2", byKey["initiative.theme_slots"])
	}
	if byKey["initiative.saved_rosters"]["usage"] != float64(0) {
		t.Errorf("saved_rosters usage = %v, want 0", byKey["initiative.saved_rosters"]["usage"])
	}
	if _, has := byKey["initiative.sessions_per_gm"]["usage"]; has {
		t.Error("sessions_per_gm has no stored resource yet; usage must be omitted")
	}
	if byKey["initiative.deluxe"]["value"] != true {
		t.Errorf("flag value = %v", byKey["initiative.deluxe"]["value"])
	}
}

func TestEntitlementsAreOwnerScoped(t *testing.T) {
	r := entitlementsRouter(t, 200)
	call(r, "POST", "/themes", theme("A"))
	// A second user's usage must not include the first user's themes.
	req := httptest.NewRequest(http.MethodGet, "/entitlements", nil)
	req.Header.Set("Authorization", "Bearer user-two")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	for _, it := range decodeBody(t, w)["items"].([]any) {
		m := it.(map[string]any)
		if m["key"] == "initiative.theme_slots" && m["usage"] != float64(0) {
			t.Fatalf("usage = %v, want 0 for a different user", m["usage"])
		}
	}
}

func TestEntitlementsUnavailableIs503(t *testing.T) {
	w := call(entitlementsRouter(t, 502), "GET", "/entitlements", nil)
	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "entitlements_unavailable" {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}
