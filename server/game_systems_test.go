package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// systemsRouter seeds No System plus one gated system and serves an entitlements-api that answers
// the game_systems_extra flag with have (status 502 when down).
func systemsRouter(t *testing.T, have string, down bool) *gin.Engine {
	t.Helper()
	db := fake.NewDB()
	seedSystem(t, db)
	gated := data.GameSystem{ID: "dnd5e", Name: "D&D 5e", Family: "d20", Aliases: []string{"dnd5e-foundry"}, Gated: true,
		Fields: []data.TrackerField{{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 10}}}
	if _, err := data.NewSystemRepo(db).Upsert(context.Background(), gated); err != nil {
		t.Fatal(err)
	}
	ent := func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Query().Get("key") == domain.KeyIntegrationCredentials {
			_, _ = w.Write([]byte(`{"key":"` + domain.KeyIntegrationCredentials + `","type":"limit","value":5,"ttl_seconds":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"key":"` + domain.KeyGameSystemsExtra + `","type":"flag","value":` + have + `,"ttl_seconds":0}`))
	}
	r, _ := credRouterEnt(t, db, allowRelations, ent)
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

func TestGameSystemGetUnknownIs404AndViewHidesStorageFields(t *testing.T) {
	r := systemsRouter(t, "false", false)

	missing := callAs(r, "user-ok", "GET", "/game-systems/nope", "")
	sys := decodeBody(t, callAs(r, "user-ok", "GET", "/game-systems/none", ""))

	if missing.Code != 404 {
		t.Errorf("unknown id = %d, want 404", missing.Code)
	}
	for _, k := range []string{"revision", "hash", "content_hash", "created_at"} {
		if _, ok := sys[k]; ok {
			t.Errorf("response exposes storage field %q", k)
		}
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

func TestAnIntegrationCredentialWithNoRecordedFlagSeesNoSystemOnly(t *testing.T) {
	r := systemsRouter(t, "true", false)
	_, secret := issue(t, r, "user-ok", `["read"]`)

	if got := systemIDs(t, r, secret, "/game-systems"); len(got) != 1 || got[0] != data.DefaultSystemID {
		t.Fatalf("credential list = %v, want only No System", got)
	}
}
