package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// credRouter serves the real handlers plus a probe route that echoes the principal. limit is the
// entitled credential count.
func credRouter(t *testing.T, limit string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"key":"initiative.integration_credentials","type":"limit","value":` + limit + `,"ttl_seconds":0}`))
	}))
	t.Cleanup(ent.Close)
	db := fake.NewDB()
	themes, _ := domain.NewThemes(db)
	svc := Services{
		Authz:        authz.NewClient(fakeAuthAPI(t).URL, fakeUsersAPI(t).URL),
		Entitlements: entitlements.New(ent.URL),
		Themes:       themes, Credentials: domain.NewCredentials(db),
		Rosters: domain.NewRosters(db), Encounters: domain.NewEncounters(db),
	}
	r := gin.New()
	r.Use(Correlation())
	SetupHandlers(r, svc)
	probe := ProtectedGroup(r, svc.Authz, svc.Credentials)
	probe.Any("/probe", func(c *gin.Context) { c.String(http.StatusOK, authz.Viewer(c)) })
	return r
}

func callAs(r http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// issue creates a credential as token's user and returns its id and secret.
func issue(t *testing.T, r http.Handler, token, scopes string) (id, secret string) {
	t.Helper()
	w := callAs(r, token, "POST", "/credentials", `{"label":"foundry","scopes":`+scopes+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	b := decodeBody(t, w)
	return b["credential"].(map[string]any)["id"].(string), b["secret"].(string)
}

func TestCredentialSecretShownOnceAndNeverStored(t *testing.T) {
	r := credRouter(t, "3")
	_, secret := issue(t, r, "user-ok", `["read"]`)
	if !strings.HasPrefix(secret, "ic_") {
		t.Fatalf("secret = %q", secret)
	}
	w := callAs(r, "user-ok", "GET", "/credentials", "")
	if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "secret_hash") {
		t.Fatalf("list leaks secret material: %s", w.Body)
	}
}

func TestCredentialActsAsOwner(t *testing.T) {
	r := credRouter(t, "3")
	_, secret := issue(t, r, "user-two", `["read"]`)
	w := callAs(r, secret, "GET", "/probe", "")
	if w.Code != 200 || w.Body.String() != "u-2" {
		t.Fatalf("probe = %d %q, want 200 u-2", w.Code, w.Body)
	}
}

func TestCredentialListIsOwnerScoped(t *testing.T) {
	r := credRouter(t, "3")
	issue(t, r, "user-ok", `["read"]`)
	w := callAs(r, "user-two", "GET", "/credentials", "")
	if items := decodeBody(t, w)["items"].([]any); len(items) != 0 {
		t.Fatalf("user-two sees %d of user-ok's credentials", len(items))
	}
}

func TestCredentialRevokeIsOwnerScopedAndImmediate(t *testing.T) {
	r := credRouter(t, "3")
	id, secret := issue(t, r, "user-ok", `["read"]`)
	if w := callAs(r, "user-two", "POST", "/credentials/"+id+"/revoke", ""); w.Code != http.StatusForbidden {
		t.Fatalf("other owner's revoke = %d, want 403", w.Code)
	}
	if w := callAs(r, secret, "GET", "/probe", ""); w.Code != 200 {
		t.Fatalf("still-live credential = %d", w.Code)
	}
	if w := callAs(r, "user-ok", "POST", "/credentials/"+id+"/revoke", ""); w.Code != 200 {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if w := callAs(r, secret, "GET", "/probe", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential = %d, want 401 on the next request", w.Code)
	}
	if w := callAs(r, "user-ok", "POST", "/credentials/"+id+"/revoke", ""); w.Code != 200 {
		t.Fatalf("repeat revoke = %d, want 200", w.Code)
	}
}

func TestCredentialScopes(t *testing.T) {
	r := credRouter(t, "3")
	_, ro := issue(t, r, "user-ok", `["read"]`)
	_, wo := issue(t, r, "user-ok", `["write"]`)
	_, both := issue(t, r, "user-ok", `["read","write"]`)
	tests := []struct {
		name, secret, method string
		want                 int
	}{
		{"read-only reads", ro, "GET", 200},
		{"read-only cannot write", ro, "POST", 403},
		{"write-only writes", wo, "PATCH", 200},
		{"write-only cannot read", wo, "GET", 403},
		{"both read", both, "GET", 200},
		{"both write", both, "DELETE", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := callAs(r, tt.secret, tt.method, "/probe", ""); w.Code != tt.want {
				t.Fatalf("%s = %d, want %d", tt.method, w.Code, tt.want)
			}
		})
	}
}

func TestCredentialsCannotManageAccountRoutes(t *testing.T) {
	r := credRouter(t, "3")
	_, secret := issue(t, r, "user-ok", `["read","write"]`)
	for _, p := range []string{"/credentials", "/entitlements", "/themes"} {
		if w := callAs(r, secret, "GET", p, ""); w.Code != http.StatusForbidden {
			t.Errorf("GET %s with credential = %d, want 403", p, w.Code)
		}
	}
}

func TestCredentialLimitIsTypedEntitlementError(t *testing.T) {
	r := credRouter(t, "1")
	id, _ := issue(t, r, "user-ok", `["read"]`)
	w := callAs(r, "user-ok", "POST", "/credentials", `{"label":"b","scopes":["read"]}`)
	b := decodeBody(t, w)
	if w.Code != 403 || b["code"] != "entitlement_limit_reached" || b["key"] != "initiative.integration_credentials" {
		t.Fatalf("over limit = %d %v", w.Code, b)
	}
	callAs(r, "user-ok", "POST", "/credentials/"+id+"/revoke", "")
	if w := callAs(r, "user-ok", "POST", "/credentials", `{"label":"b","scopes":["read"]}`); w.Code != 201 {
		t.Fatalf("after revoke = %d, want slot freed", w.Code)
	}
}

func TestCredentialRejectsBadInput(t *testing.T) {
	r := credRouter(t, "3")
	for _, body := range []string{`{"label":"","scopes":["read"]}`, `{"label":"x","scopes":[]}`, `{"label":"x","scopes":["admin"]}`} {
		if w := callAs(r, "user-ok", "POST", "/credentials", body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", body, w.Code)
		}
	}
}

func TestUnknownCredentialSecretIs401(t *testing.T) {
	if w := callAs(credRouter(t, "3"), "ic_nope", "GET", "/probe", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", w.Code)
	}
}

func TestCombatantLookupNeedsBothParamsAndReadScope(t *testing.T) {
	r := credRouter(t, "3")
	_, wo := issue(t, r, "user-ok", `["write"]`)
	_, ro := issue(t, r, "user-ok", `["read"]`)
	if w := callAs(r, ro, "GET", "/combatants?ref_key=foundry.actor", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing ref_value = %d, want 422", w.Code)
	}
	if w := callAs(r, wo, "GET", "/combatants?ref_key=a&ref_value=b", ""); w.Code != http.StatusForbidden {
		t.Errorf("write-only lookup = %d, want 403", w.Code)
	}
}
