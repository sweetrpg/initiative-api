package server

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// sysEnt is a switchable entitlements-api: the game_systems_extra flag, an outage switch, and a
// count of the calls it served.
type sysEnt struct {
	have, down atomic.Bool
	calls      atomic.Int64
	ttl        atomic.Int64 // seconds the flag may be cached; 0 means not cached
}

func (e *sysEnt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.calls.Add(1)
	if e.down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.URL.Query().Get("key") == domain.KeyIntegrationCredentials {
		_, _ = w.Write([]byte(`{"key":"` + domain.KeyIntegrationCredentials + `","type":"limit","value":5,"ttl_seconds":0}`))
		return
	}
	_, _ = w.Write([]byte(`{"key":"` + domain.KeyGameSystemsExtra + `","type":"flag","value":` + strconv.FormatBool(e.have.Load()) + `,"ttl_seconds":` + strconv.FormatInt(e.ttl.Load(), 10) + `}`))
}

// systemsRouterEnt seeds No System plus one gated system (alias dnd5e-foundry) and returns the
// router with its switchable entitlements-api.
func systemsRouterEnt(t *testing.T) (*gin.Engine, *sysEnt) {
	t.Helper()
	db := fake.NewDB()
	seedSystem(t, db)
	gated := data.GameSystem{ID: "dnd5e", Name: "D&D 5e", Family: "d20", Aliases: []string{"dnd5e-foundry"}, Gated: true,
		Fields: []data.TrackerField{{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 10}}}
	if _, err := data.NewSystemRepo(db).Upsert(context.Background(), gated); err != nil {
		t.Fatal(err)
	}
	ent := &sysEnt{}
	r, _ := credRouterEnt(t, db, allowRelations, ent.ServeHTTP)
	return r, ent
}

// systemsRouter is systemsRouterEnt with the flag and outage set up front.
func systemsRouter(t *testing.T, have string, down bool) *gin.Engine {
	t.Helper()
	r, ent := systemsRouterEnt(t)
	ent.have.Store(have == "true")
	ent.down.Store(down)
	return r
}

func systemIDs(t *testing.T, r http.Handler, token, path string) []string {
	t.Helper()
	w := callAs(r, token, "GET", path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
	}
	var ids []string
	for _, it := range decodeBody(t, w)["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func TestGameSystemListShowsGatedSystemsOnlyToTheEntitled(t *testing.T) {
	tests := []struct {
		name string
		r    *gin.Engine
		want int
	}{
		{"entitled", systemsRouter(t, "true", false), 2},
		{"not entitled", systemsRouter(t, "false", false), 1},
		{"entitlements down reads as not entitled", systemsRouter(t, "true", true), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemIDs(t, tt.r, "user-ok", "/game-systems"); len(got) != tt.want {
				t.Fatalf("ids = %v, want %d", got, tt.want)
			}
		})
	}
}

func TestAFilterCannotWidenWhatAnUnentitledCallerSees(t *testing.T) {
	r := systemsRouter(t, "false", false)

	if got := systemIDs(t, r, "user-ok", "/game-systems?filter[gated]=true"); len(got) != 0 {
		t.Fatalf("gated=true as an unentitled caller = %v, want none", got)
	}
}

func TestGameSystemGetResolvesAnAliasAndGatesTheLockedOne(t *testing.T) {
	entitled, plain := systemsRouter(t, "true", false), systemsRouter(t, "false", false)

	alias := callAs(plain, "user-ok", "GET", "/game-systems/none", "")
	locked := callAs(plain, "user-ok", "GET", "/game-systems/dnd5e", "")
	open := callAs(entitled, "user-ok", "GET", "/game-systems/dnd5e-foundry", "")

	if alias.Code != 200 || decodeBody(t, alias)["id"] != data.DefaultSystemID {
		t.Errorf("ungated by alias = %d %s", alias.Code, alias.Body)
	}
	if b := decodeBody(t, locked); locked.Code != http.StatusForbidden || b["code"] != "entitlement_required" || b["key"] != domain.KeyGameSystemsExtra {
		t.Errorf("gated, not entitled = %d %s, want 403 entitlement_required", locked.Code, locked.Body)
	}
	if open.Code != 200 || decodeBody(t, open)["id"] != "dnd5e" {
		t.Errorf("gated, entitled, by alias = %d %s", open.Code, open.Body)
	}
}

func TestGatedGameSystemGetIs503WhenTheFlagCannotBeRead(t *testing.T) {
	r := systemsRouter(t, "true", true)

	w := callAs(r, "user-ok", "GET", "/game-systems/dnd5e", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("gated get with entitlements down = %d %s, want 503", w.Code, w.Body)
	}
}

func TestGameSystemGetUnknownIs404AndViewIsExactlyTheDefinition(t *testing.T) {
	r := systemsRouter(t, "false", false)

	missing := callAs(r, "user-ok", "GET", "/game-systems/nope", "")
	sys := decodeBody(t, callAs(r, "user-ok", "GET", "/game-systems/none", ""))

	if missing.Code != 404 {
		t.Errorf("unknown id = %d, want 404", missing.Code)
	}
	keys := make([]string, 0, len(sys))
	for k := range sys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"aliases", "family", "fields", "gated", "id", "name"}; !slices.Equal(keys, want) {
		t.Errorf("response keys = %v, want exactly %v", keys, want)
	}
}

func TestGameSystemFieldsProjectionAndRejection(t *testing.T) {
	r := systemsRouter(t, "true", false)

	proj := decodeBody(t, callAs(r, "user-ok", "GET", "/game-systems?fields=name", ""))["items"].([]any)[0].(map[string]any)
	bad := callAs(r, "user-ok", "GET", "/game-systems?fields=revision", "")

	if len(proj) != 2 || proj["name"] == nil || proj["id"] == nil {
		t.Errorf("fields=name = %v, want id and name only", proj)
	}
	if bad.Code != 422 {
		t.Errorf("fields=revision = %d, want 422", bad.Code)
	}
}

func TestGatedGetUsesNoStaleFallbackSoAnOutageIs503(t *testing.T) {
	r, ent := systemsRouterEnt(t)
	ent.have.Store(true)
	ent.ttl.Store(1)
	if w := callAs(r, "user-ok", "GET", "/game-systems/dnd5e", ""); w.Code != 200 { // caches the flag for 1s
		t.Fatalf("entitled get = %d %s", w.Code, w.Body)
	}
	ent.down.Store(true)
	time.Sleep(1100 * time.Millisecond) // the cached value is now expired but still within the stale window

	w := callAs(r, "user-ok", "GET", "/game-systems/dnd5e", "")

	if b := decodeBody(t, w); w.Code != http.StatusServiceUnavailable || b["code"] != "entitlements_unavailable" {
		t.Fatalf("gated get after the service went down = %d %s, want 503 from a fail-closed check", w.Code, w.Body)
	}
}

func TestADegradedListIsMarkedAndNeverCached(t *testing.T) {
	r, ent := systemsRouterEnt(t)
	ent.down.Store(true)

	w := callAs(r, "user-ok", "GET", "/game-systems", "")
	get := callAs(r, "user-ok", "GET", "/game-systems/none", "")

	if w.Code != 200 || w.Header().Get("X-Entitlements-Degraded") != "true" {
		t.Errorf("degraded list = %d, header %q; want 200 marked degraded", w.Code, w.Header().Get("X-Entitlements-Degraded"))
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || get.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("Cache-Control = %q / %q, want private, no-store on both routes", w.Header().Get("Cache-Control"), get.Header().Get("Cache-Control"))
	}
}

func TestAnUnentitledCallerUsingTheFoundryAliasGets403(t *testing.T) {
	r := systemsRouter(t, "false", false)

	w := callAs(r, "user-ok", "GET", "/game-systems/dnd5e-foundry", "")

	if b := decodeBody(t, w); w.Code != http.StatusForbidden || b["code"] != "entitlement_required" {
		t.Fatalf("gated alias, not entitled = %d %s, want 403", w.Code, w.Body)
	}
}

// credentialSees seeds the owner's record through a first-party list, then issues a read
// credential. It returns the secret and the entitlements calls made so far.
func credentialAfterRecord(t *testing.T, r http.Handler, ent *sysEnt) (secret string, callsBefore int64) {
	t.Helper()
	_ = callAs(r, "user-ok", "GET", "/game-systems", "") // first-party read records the flag
	_, secret = issue(t, r, "user-ok", `["read"]`)
	return secret, ent.calls.Load()
}

func TestACredentialWithARecordedTrueSeesAndGetsGatedSystemsWithoutCallingEntitlements(t *testing.T) {
	r, ent := systemsRouterEnt(t)
	ent.have.Store(true)
	secret, before := credentialAfterRecord(t, r, ent)

	list := systemIDs(t, r, secret, "/game-systems")
	byID := callAs(r, secret, "GET", "/game-systems/dnd5e", "")
	byAlias := callAs(r, secret, "GET", "/game-systems/dnd5e-foundry", "")

	if len(list) != 2 || byID.Code != 200 || byAlias.Code != 200 {
		t.Errorf("list = %v, get = %d, alias = %d; want 2 systems and 200s", list, byID.Code, byAlias.Code)
	}
	if got := ent.calls.Load(); got != before {
		t.Errorf("entitlements served %d calls while credential requests ran, want 0", got-before)
	}
}

func TestACredentialWithARecordedFalseGets403OnAGatedGet(t *testing.T) {
	r, ent := systemsRouterEnt(t)
	secret, _ := credentialAfterRecord(t, r, ent)

	w := callAs(r, secret, "GET", "/game-systems/dnd5e", "")

	if b := decodeBody(t, w); w.Code != http.StatusForbidden || b["code"] != "entitlement_required" {
		t.Fatalf("credential, recorded false = %d %s, want 403", w.Code, w.Body)
	}
}

func TestACredentialWithNoRecordGets503OnAGatedGetButAListStillWorks(t *testing.T) {
	r, _ := systemsRouterEnt(t)
	_, secret := issue(t, r, "user-ok", `["read"]`) // no first-party read has recorded the flag

	get := callAs(r, secret, "GET", "/game-systems/dnd5e", "")
	list := systemIDs(t, r, secret, "/game-systems")

	if b := decodeBody(t, get); get.Code != http.StatusServiceUnavailable || b["code"] != "entitlements_unavailable" {
		t.Errorf("credential, no record, gated get = %d %s, want 503 entitlements_unavailable", get.Code, get.Body)
	}
	if len(list) != 1 || list[0] != data.DefaultSystemID {
		t.Errorf("credential list with no record = %v, want only No System", list)
	}
}
