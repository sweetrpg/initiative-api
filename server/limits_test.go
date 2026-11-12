package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// limitRig serves the real handlers with the limit enforcer over a fake database. limits maps an
// entitlement key to the value the fake entitlements-api answers; down makes it answer 502.
type limitRig struct {
	r      *gin.Engine
	mu     sync.Mutex
	limits map[string]int
	down   bool
}

func (l *limitRig) set(key string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits[key] = n
}

func newLimitRig(t *testing.T) *limitRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reconcileEvery = 0
	t.Cleanup(func() { reconcileEvery = 30 * time.Second })
	l := &limitRig{limits: map[string]int{}}
	ent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		key := r.URL.Query().Get("key")
		if l.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"key":"` + key + `","type":"limit","value":` + strconv.Itoa(l.limits[key]) + `,"ttl_seconds":0}`))
	}))
	t.Cleanup(ent.Close)
	db := fake.NewDB()
	themes, _ := domain.NewThemes(db)
	creds := domain.NewCredentials(db)
	if ok, err := creds.Open(context.Background()); err != nil || !ok {
		t.Fatalf("open = %v, %v", ok, err)
	}
	rosters, encounters := domain.NewRosters(db), domain.NewEncounters(db)
	svc := Services{
		Authz: authz.NewClient(fakeAuthAPI(t).URL, fakeUsersAPI(t).URL), Entitlements: entitlements.New(ent.URL),
		Themes: themes, Credentials: creds, Rosters: rosters, Encounters: encounters,
		Limits: domain.NewLimits(
			domain.Kind{Name: "themes", Key: domain.KeyThemeSlots, Repo: themes.Items()},
			domain.Kind{Name: "credentials", Key: domain.KeyIntegrationCredentials, Repo: creds.Items()},
		),
	}
	l.r = gin.New()
	l.r.Use(Correlation())
	SetupHandlers(l.r, svc)
	probe := ProtectedGroup(l.r, svc.Authz, svc.Credentials)
	probe.GET("/probe", func(c *gin.Context) { c.String(http.StatusOK, authz.Viewer(c)) })
	return l
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (l *limitRig) call(token, method, path, body string) *httptest.ResponseRecorder {
	return callAs(l.r, token, method, path, body)
}

// themes creates n themes as user-ok and returns their IDs in creation order.
func (l *limitRig) themes(t *testing.T, n int) []string {
	t.Helper()
	l.set(domain.KeyThemeSlots, n)
	ids := make([]string, n)
	for i := range ids {
		w := l.call("user-ok", "POST", "/themes", jsonOf(theme(string(rune('A'+i)))))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s", i, w.Code, w.Body)
		}
		ids[i] = decodeBody(t, w)["id"].(string)
	}
	return ids
}

// states maps theme ID to whether it is inactive, read through the list endpoint (which also
// triggers a reconcile, as every request does).
func (l *limitRig) states(t *testing.T) map[string]bool {
	t.Helper()
	items := decodeBody(t, l.call("user-ok", "GET", "/themes", ""))["items"].([]any)
	m := map[string]bool{}
	for _, it := range items {
		o := it.(map[string]any)
		_, inactive := o["inactive_at"]
		m[o["id"].(string)] = inactive
	}
	return m
}

func countInactive(m map[string]bool) (n int) {
	for _, in := range m {
		if in {
			n++
		}
	}
	return n
}

func TestDowngradeKeepsOldestActiveAndRemovesNothing(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 3)
	l.set(domain.KeyThemeSlots, 1)
	l.call("user-ok", "GET", "/themes", "") // the request that applies the new limit
	m := l.states(t)
	if len(m) != 3 || m[ids[0]] || !m[ids[1]] || !m[ids[2]] {
		t.Fatalf("states = %v, want oldest active and the other two inactive, none removed", m)
	}
}

func TestInactiveItemIsReadableAndDeletableNotEditable(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 2)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	if w := l.call("user-ok", "GET", "/themes/"+ids[1], ""); w.Code != 200 {
		t.Fatalf("read inactive = %d", w.Code)
	}
	w := l.call("user-ok", "PATCH", "/themes/"+ids[1], `{"revision":1,"name":"x"}`)
	if w.Code != http.StatusForbidden || decodeBody(t, w)["code"] != "item_inactive" {
		t.Fatalf("edit inactive = %d %s", w.Code, w.Body)
	}
	if w := l.call("user-ok", "DELETE", "/themes/"+ids[1]+"?revision=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete inactive = %d", w.Code)
	}
}

func TestOwnerChoiceDecidesWhichStayActive(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 3)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	w := l.call("user-ok", "PUT", "/entitlements/active/themes", `{"ids":["`+ids[2]+`"]}`)
	if w.Code != 200 {
		t.Fatalf("choose = %d %s", w.Code, w.Body)
	}
	m := l.states(t)
	if m[ids[2]] || !m[ids[0]] || !m[ids[1]] {
		t.Fatalf("states = %v, want only the chosen theme active", m)
	}
}

func TestChooseRejectsBadSelections(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 3)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	for name, body := range map[string]string{
		"more than the limit": `{"ids":["` + ids[0] + `","` + ids[1] + `"]}`,
		"not owned":           `{"ids":["64b7f0f0f0f0f0f0f0f0f0f0"]}`,
		"duplicate":           `{"ids":["` + ids[0] + `","` + ids[0] + `"]}`,
	} {
		if w := l.call("user-ok", "PUT", "/entitlements/active/themes", body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", name, w.Code)
		}
	}
	if w := l.call("user-ok", "PUT", "/entitlements/active/nope", `{"ids":[]}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown kind = %d, want 404", w.Code)
	}
}

func TestUpgradeReactivatesInChosenOrderThenOldest(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 4)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	l.call("user-ok", "PUT", "/entitlements/active/themes", `{"ids":["`+ids[3]+`","`+ids[2]+`"]}`) // over the limit: rejected, no change
	l.call("user-ok", "PUT", "/entitlements/active/themes", `{"ids":["`+ids[3]+`"]}`)
	l.set(domain.KeyThemeSlots, 2)
	m := l.states(t)
	if m[ids[3]] || m[ids[0]] || !m[ids[1]] || !m[ids[2]] {
		t.Fatalf("states = %v, want the chosen theme then the oldest active", m)
	}
	l.set(domain.KeyThemeSlots, 4)
	if n := countInactive(l.states(t)); n != 0 {
		t.Fatalf("%d inactive after the limit covers everything", n)
	}
}

func TestDeletingAnActiveItemActivatesAnInactiveOne(t *testing.T) {
	l := newLimitRig(t)
	ids := l.themes(t, 2)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	if w := l.call("user-ok", "DELETE", "/themes/"+ids[0]+"?revision=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", w.Code)
	}
	m := l.states(t)
	if len(m) != 1 || m[ids[1]] {
		t.Fatalf("states = %v, want the remaining theme active", m)
	}
}

func TestCreatingWhileOverTheLimitCountsInactiveItems(t *testing.T) {
	l := newLimitRig(t)
	l.themes(t, 3)
	l.set(domain.KeyThemeSlots, 1)
	l.states(t)
	w := l.call("user-ok", "POST", "/themes", jsonOf(theme("New")))
	if w.Code != http.StatusForbidden || decodeBody(t, w)["code"] != "entitlement_limit_reached" {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
}

func TestEntitlementsOutageDeactivatesNothing(t *testing.T) {
	l := newLimitRig(t)
	l.themes(t, 3)
	l.mu.Lock()
	l.down = true
	l.mu.Unlock()
	if n := countInactive(l.states(t)); n != 0 {
		t.Fatalf("%d inactive during an outage", n)
	}
}

func TestInactiveCredentialStopsAuthenticating(t *testing.T) {
	l := newLimitRig(t)
	l.set(domain.KeyIntegrationCredentials, 2)
	l.set(domain.KeyThemeSlots, 3)
	issue := func() string {
		w := l.call("user-ok", "POST", "/credentials", `{"label":"x","scopes":["read"]}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("issue = %d %s", w.Code, w.Body)
		}
		return decodeBody(t, w)["secret"].(string)
	}
	first, second := issue(), issue()
	l.set(domain.KeyIntegrationCredentials, 1)
	l.call("user-ok", "GET", "/credentials", "")
	if w := l.call(first, "GET", "/probe", ""); w.Code != 200 {
		t.Fatalf("oldest credential = %d, want 200", w.Code)
	}
	if w := l.call(second, "GET", "/probe", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("inactive credential = %d, want 401", w.Code)
	}
	l.set(domain.KeyIntegrationCredentials, 2)
	l.call("user-ok", "GET", "/credentials", "")
	if w := l.call(second, "GET", "/probe", ""); w.Code != 200 {
		t.Fatalf("reactivated credential = %d, want 200", w.Code)
	}
}
