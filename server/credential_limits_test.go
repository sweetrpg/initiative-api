package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// strictEntitlements is an entitlements-api that, like the real one, refuses a request with no
// user bearer. It answers every key with limit, and counts the calls it served.
func strictEntitlements(limit string, calls *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer")) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls.Add(1)
		key := r.URL.Query().Get("key")
		if strings.HasPrefix(r.URL.Path, "/check/batch") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"key": "initiative.saved_rosters", "type": "limit", "value": json.Number(limit), "ttl_seconds": 0},
				{"key": "initiative.saved_encounters", "type": "limit", "value": json.Number(limit), "ttl_seconds": 0},
			}})
			return
		}
		_, _ = w.Write([]byte(`{"key":"` + key + `","type":"limit","value":` + limit + `,"ttl_seconds":0}`))
	}
}

func strictRouter(t *testing.T, limit string) (*gin.Engine, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	db := fake.NewDB()
	seedSystem(t, db)
	r, _ := credRouterEnt(t, db, allowRelations, strictEntitlements(limit, &calls))
	return r, &calls
}

func creds(t *testing.T, r http.Handler) string {
	t.Helper()
	_, secret := issue(t, r, "user-ok", `["read","write"]`)
	return secret
}

func TestACredentialCreateWithNoRecordedLimitsFailsClosed(t *testing.T) {
	r, _ := strictRouter(t, "3")
	secret := creds(t, r)

	w := callAs(r, secret, "POST", "/rosters", `{"name":"R"}`)

	if b := decodeBody(t, w); w.Code != http.StatusServiceUnavailable || b["code"] != "entitlements_unavailable" || !strings.Contains(b["message"].(string), "once") {
		t.Fatalf("credential create with no record = %d %s, want 503 entitlements_unavailable telling the owner to open the app", w.Code, w.Body)
	}
}

func TestACredentialCreateWorksOffTheRecordAndNeverCallsEntitlements(t *testing.T) {
	r, calls := strictRouter(t, "2")
	secret := creds(t, r)
	if w := callAs(r, "user-ok", "POST", "/rosters", `{"name":"First"}`); w.Code != 201 { // first-party: records the value
		t.Fatalf("first-party create = %d %s", w.Code, w.Body)
	}
	before := calls.Load()

	ok := callAs(r, secret, "POST", "/rosters", `{"name":"Second"}`)
	atLimit := callAs(r, secret, "POST", "/rosters", `{"name":"Third"}`)

	if ok.Code != 201 {
		t.Fatalf("credential create within the limit = %d %s", ok.Code, ok.Body)
	}
	if b := decodeBody(t, atLimit); atLimit.Code != 403 || b["code"] != "entitlement_limit_reached" || b["key"] != "initiative.saved_rosters" || b["limit"] != float64(2) {
		t.Errorf("credential create at the limit = %d %s", atLimit.Code, atLimit.Body)
	}
	if calls.Load() != before {
		t.Errorf("entitlements-api was called %d times on the credential's behalf; a credential has no user bearer", calls.Load()-before)
	}
}

func TestTheUsageEndpointRecordsLimitsForCredentials(t *testing.T) {
	r, _ := strictRouter(t, "4")
	secret := creds(t, r)
	if w := callAs(r, secret, "POST", "/encounters", `{"name":"E","game_system_id":"none"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("before any first-party read = %d, want 503", w.Code)
	}

	if w := callAs(r, "user-ok", "GET", "/entitlements", ""); w.Code != 200 {
		t.Fatalf("usage = %d %s", w.Code, w.Body)
	}
	w := callAs(r, secret, "POST", "/encounters", `{"name":"E","game_system_id":"none"}`)

	if w.Code != 201 {
		t.Fatalf("credential create after the usage endpoint recorded the limits = %d %s", w.Code, w.Body)
	}
}

func TestRecordedLimitsBelongToTheOwnerOnly(t *testing.T) {
	r, _ := strictRouter(t, "3")
	if w := callAs(r, "user-two", "POST", "/rosters", `{"name":"Theirs"}`); w.Code != 201 { // records for u-2
		t.Fatalf("user-two create = %d", w.Code)
	}
	secret := creds(t, r) // user-ok's credential

	w := callAs(r, secret, "POST", "/rosters", `{"name":"Mine"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("credential of a user with no record = %d %s, want 503 even though another user's limits are recorded", w.Code, w.Body)
	}
}
