package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// sessionTable has user-ok (u-1) as GM, user-two (u-2) as a player; user-three (u-3) is outside.
func sessionTable(t *testing.T) (r http.Handler, id string) {
	t.Helper()
	router, _ := credRouterWith(t, "3")
	w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	id = decodeBody(t, w)["id"].(string)
	code := inviteCode(t, router, "user-ok", id)
	w = callAs(router, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("join = %d %s", w.Code, w.Body)
	}
	reqID := decodeBody(t, w)["id"].(string)
	if w := callAs(router, "user-ok", "POST", "/sessions/"+id+"/requests/"+reqID+"/approve", `{"role":"player"}`); w.Code != 200 {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	return router, id
}

func inviteCode(t *testing.T, r http.Handler, token, id string) string {
	t.Helper()
	w := callAs(r, token, "POST", "/sessions/"+id+"/invites", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("invite = %d %s", w.Code, w.Body)
	}
	return decodeBody(t, w)["code"].(string)
}

func TestSessionLifecycleOverHTTP(t *testing.T) {
	r, id := sessionTable(t)

	get := callAs(r, "user-two", "GET", "/sessions/"+id, "")
	list := callAs(r, "user-two", "GET", "/sessions", "")
	members := callAs(r, "user-two", "GET", "/sessions/"+id+"/members", "")

	if get.Code != 200 || decodeBody(t, get)["role"] != "player" {
		t.Fatalf("get = %d %s", get.Code, get.Body)
	}
	if n := len(decodeBody(t, list)["items"].([]any)); n != 1 {
		t.Fatalf("player lists %d sessions", n)
	}
	items := decodeBody(t, members)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("members = %v", items)
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["user_id"] == nil || m["role"] == nil || m["email"] != nil {
			t.Fatalf("member entry = %v, want user_id and role and no email", m)
		}
	}
}

func TestNonMemberAndPendingUserGetNothing(t *testing.T) {
	r, id := sessionTable(t)
	code := inviteCode(t, r, "user-ok", id)
	w := callAs(r, "user-three", "POST", "/sessions/join", `{"code":"`+code+`"}`)

	for _, path := range []string{"/sessions/" + id, "/sessions/" + id + "/members", "/sessions/" + id + "/requests"} {
		if got := callAs(r, "user-three", "GET", path, ""); got.Code != http.StatusForbidden {
			t.Errorf("pending user GET %s = %d, want 403", path, got.Code)
		}
	}
	body := decodeBody(t, w)
	if w.Code != http.StatusAccepted || body["status"] != "pending" || body["session_id"] != nil || body["name"] != nil {
		t.Fatalf("join = %d %v, want only an id and a pending state", w.Code, body)
	}
}

func TestPlayerCannotManage(t *testing.T) {
	r, id := sessionTable(t)
	routes := [][3]string{
		{"POST", "/sessions/" + id + "/invites", ""},
		{"GET", "/sessions/" + id + "/invites", ""},
		{"GET", "/sessions/" + id + "/requests", ""},
		{"DELETE", "/sessions/" + id + "/members/u-1", ""},
		{"PATCH", "/sessions/" + id + "/members/u-1", `{"role":"observer"}`},
		{"POST", "/sessions/" + id + "/blocks", `{"user_id":"u-3"}`},
		{"DELETE", "/sessions/" + id + "?revision=1", ""},
	}
	for _, rt := range routes {
		if w := callAs(r, "user-two", rt[0], rt[1], rt[2]); w.Code != http.StatusForbidden {
			t.Errorf("player %s %s = %d, want 403", rt[0], rt[1], w.Code)
		}
	}
	if w := callAs(r, "user-ok", "GET", "/sessions/"+id, ""); w.Code != 200 {
		t.Fatalf("session after refused calls = %d", w.Code)
	}
}

func TestGMDeletesSessionAndMembersLoseAccess(t *testing.T) {
	r, id := sessionTable(t)
	v := decodeBody(t, callAs(r, "user-ok", "GET", "/sessions/"+id, ""))

	w := callAs(r, "user-ok", "DELETE", "/sessions/"+id+"?revision=1", "")

	if v["revision"] != float64(1) || w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if w := callAs(r, "user-two", "GET", "/sessions/"+id, ""); w.Code != http.StatusNotFound {
		t.Fatalf("former member = %d, want 404", w.Code)
	}
}

func TestRemoveAndBlockOverHTTP(t *testing.T) {
	r, id := sessionTable(t)
	code := inviteCode(t, r, "user-ok", id)

	if w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/blocks", `{"user_id":"u-2"}`); w.Code != http.StatusNoContent {
		t.Fatalf("block = %d %s", w.Code, w.Body)
	}

	if w := callAs(r, "user-two", "GET", "/sessions/"+id, ""); w.Code != http.StatusForbidden {
		t.Errorf("blocked member = %d, want 403", w.Code)
	}
	w := callAs(r, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`)
	if w.Code != http.StatusUnprocessableEntity || decodeBody(t, w)["code"] != "invalid" {
		t.Errorf("blocked join = %d %s", w.Code, w.Body)
	}
	blocks := decodeBody(t, callAs(r, "user-ok", "GET", "/sessions/"+id+"/blocks", ""))["items"].([]any)
	if len(blocks) != 1 {
		t.Errorf("blocks = %v", blocks)
	}
	if w := callAs(r, "user-ok", "DELETE", "/sessions/"+id+"/blocks/u-2", ""); w.Code != http.StatusNoContent {
		t.Errorf("unblock = %d", w.Code)
	}
}

func TestInviteRevokeRejectAndLeaveOverHTTP(t *testing.T) {
	r, id := sessionTable(t)
	w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/invites", `{"ttl_hours":2}`)
	inv := decodeBody(t, w)
	invID := inv["invite"].(map[string]any)["id"].(string)
	code := inv["code"].(string)

	if w := callAs(r, "user-ok", "DELETE", "/sessions/"+id+"/invites/"+invID, ""); w.Code != 200 {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if w := callAs(r, "user-three", "POST", "/sessions/join", `{"code":"`+code+`"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("revoked invite = %d, want 422", w.Code)
	}
	live := inviteCode(t, r, "user-ok", id)
	reqID := decodeBody(t, callAs(r, "user-three", "POST", "/sessions/join", `{"code":"`+live+`"}`))["id"].(string)
	if w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/requests/"+reqID+"/reject", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("reject without reason = %d, want 422", w.Code)
	}
	if w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/requests/"+reqID+"/reject", `{"reason":"no"}`); w.Code != 200 {
		t.Errorf("reject = %d %s", w.Code, w.Body)
	}
	if w := callAs(r, "user-two", "POST", "/sessions/"+id+"/leave", ""); w.Code != http.StatusNoContent {
		t.Errorf("leave = %d", w.Code)
	}
	if w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/leave", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("GM leave = %d, want 422", w.Code)
	}
}

func TestSessionRoutesRejectBadInput(t *testing.T) {
	r, id := sessionTable(t)
	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"create without a name", "POST", "/sessions", `{}`, http.StatusUnprocessableEntity},
		{"create with bad json", "POST", "/sessions", `nope`, http.StatusBadRequest},
		{"join without a code", "POST", "/sessions/join", `{}`, http.StatusBadRequest},
		{"delete without revision", "DELETE", "/sessions/" + id, "", http.StatusBadRequest},
		{"bad limit", "GET", "/sessions?limit=abc", "", http.StatusBadRequest},
		{"unknown session", "GET", "/sessions/64b7f0f0f0f0f0f0f0f0f0f0", "", http.StatusNotFound},
		{"set role with bad json", "PATCH", "/sessions/" + id + "/members/u-2", `x`, http.StatusBadRequest},
		{"set role to gm", "PATCH", "/sessions/" + id + "/members/u-2", `{"role":"gm"}`, http.StatusUnprocessableEntity},
		{"bad invite lifetime", "POST", "/sessions/" + id + "/invites", `{"ttl_hours":1000}`, http.StatusUnprocessableEntity},
		{"approve with no role", "POST", "/sessions/" + id + "/requests/x/approve", `{}`, http.StatusUnprocessableEntity},
		{"block with a non-id", "POST", "/sessions/" + id + "/blocks", `{"user_id":"a b"}`, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := callAs(r, "user-ok", tt.method, tt.path, tt.body); w.Code != tt.want {
				t.Fatalf("%s %s = %d %s, want %d", tt.method, tt.path, w.Code, w.Body, tt.want)
			}
		})
	}
}

func TestIntegrationCredentialsCannotUseSessions(t *testing.T) {
	r, id := sessionTable(t)
	_, secret := issue(t, r, "user-ok", `["read","write"]`)
	for _, path := range []string{"/sessions", "/sessions/" + id} {
		w := callAs(r, secret, "GET", path, "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "user_token_required") {
			t.Errorf("credential GET %s = %d %s, want 403 user_token_required", path, w.Code, w.Body)
		}
	}
}

func TestSecondSessionOverTheGMLimitIsTyped403(t *testing.T) {
	r, _ := credRouterWith(t, "1")
	if w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"One"}`); w.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", w.Code, w.Body)
	}

	w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"Two"}`)

	body := decodeBody(t, w)
	if w.Code != http.StatusForbidden || body["code"] != "entitlement_limit_reached" || body["key"] != "initiative.sessions_per_gm" {
		t.Fatalf("second = %d %v", w.Code, body)
	}
}

func sessionRouterWith(t *testing.T, rel http.HandlerFunc) http.Handler {
	t.Helper()
	r, _ := credRouterRel(t, "3", fake.NewDB(), rel)
	return r
}

func TestSessionRoutesAnswer503WhenAuthorizationIsDown(t *testing.T) {
	var down atomic.Bool
	router := sessionRouterWith(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "openfga down", http.StatusBadGateway)
			return
		}
		allowRelations(w, r)
	})
	w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	id := decodeBody(t, w)["id"].(string)
	down.Store(true)

	for _, path := range []string{"/sessions/" + id, "/sessions/" + id + "/members", "/sessions"} {
		w := callAs(router, "user-ok", "GET", path, "")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "authorization_unavailable") {
			t.Errorf("GET %s = %d %s, want 503 authorization_unavailable", path, w.Code, w.Body)
		}
	}
	if w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Saturday"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("create in an outage = %d %s, want 503: the session ID is minted by auth-api", w.Code, w.Body)
	}
}

func TestPrivateRecordsWorkWhenAuthorizationIsDown(t *testing.T) {
	router := sessionRouterWith(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) })

	w := callAs(router, "user-ok", "GET", "/entitlements", "")

	if w.Code != http.StatusOK {
		t.Fatalf("entitlements in an outage = %d %s, want 200", w.Code, w.Body)
	}
}

// relProbe is a check-relation endpoint that records what was asked and can be switched to deny.
type relProbe struct {
	mu    sync.Mutex
	deny  bool
	calls []struct{ Object, Relation string }
}

func (p *relProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/authz/check-relation" {
		allowRelations(w, r)
		return
	}
	var q struct{ Object, Relation string }
	_ = json.NewDecoder(r.Body).Decode(&q)
	p.mu.Lock()
	p.calls = append(p.calls, q)
	deny := p.deny
	p.mu.Unlock()
	if deny {
		_, _ = w.Write([]byte(`{"allowed":false}`))
		return
	}
	_, _ = w.Write([]byte(`{"allowed":true}`))
}

func (p *relProbe) setDeny(v bool) {
	p.mu.Lock()
	p.deny = v
	p.mu.Unlock()
}

func (p *relProbe) last() struct{ Object, Relation string } {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1]
}

func (p *relProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func TestDeniedRelationIs403WithTheRightRelationAndNoSessionData(t *testing.T) {
	db := fake.NewDB()
	probe := &relProbe{}
	r, _ := credRouterRel(t, "3", db, probe.ServeHTTP)
	w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)
	id := decodeBody(t, w)["id"].(string)
	probe.setDeny(true)
	routes := []struct{ method, path, body, relation string }{
		{"GET", "/sessions/" + id, "", "can_view"},
		{"GET", "/sessions/" + id + "/members", "", "can_view"},
		{"GET", "/sessions/" + id + "/invites", "", "can_edit"},
		{"GET", "/sessions/" + id + "/requests", "", "can_approve"},
		{"GET", "/sessions/" + id + "/blocks", "", "can_edit"},
		{"POST", "/sessions/" + id + "/invites", "", "can_edit"},
		{"PATCH", "/sessions/" + id + "/members/user-two", `{"role":"player"}`, "can_edit"},
		{"DELETE", "/sessions/" + id + "?revision=1", "", "gm"},
	}
	for _, rt := range routes {
		w := callAs(r, "user-ok", rt.method, rt.path, rt.body)
		got := probe.last()
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"forbidden"`) || strings.Contains(w.Body.String(), "Friday") {
			t.Errorf("%s %s = %d %s, want a bare 403", rt.method, rt.path, w.Code, w.Body)
		}
		if got.Object != "session:"+id || got.Relation != rt.relation {
			t.Errorf("%s %s asked %+v, want session:%s %s", rt.method, rt.path, got, id, rt.relation)
		}
	}
	if n := len(db.Fake(data.CollectionSessionInvites).Docs()); n != 0 {
		t.Errorf("a denied write created %d invites", n)
	}
	for _, d := range db.Fake(data.CollectionSessionMembers).Docs() {
		if d["deleted_at"] != nil {
			t.Errorf("a denied delete changed a member: %v", d)
		}
	}
}

func TestJoinNeedsNoRelationCheckAndWorksInAnOutage(t *testing.T) {
	probe := &relProbe{}
	r, _ := credRouterRel(t, "3", fake.NewDB(), probe.ServeHTTP)
	id := decodeBody(t, callAs(r, "user-ok", "POST", "/sessions", `{"name":"Friday"}`))["id"].(string)
	code := inviteCode(t, r, "user-ok", id)
	probe.setDeny(true)
	before := probe.count()

	w := callAs(r, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`)

	if w.Code != http.StatusAccepted || probe.count() != before {
		t.Fatalf("join = %d %s with %d extra checks; want 202 and none", w.Code, w.Body, probe.count()-before)
	}
}

func TestWriteRoutesAnswer503WhenAuthorizationIsDown(t *testing.T) {
	var down atomic.Bool
	r := sessionRouterWith(t, func(w http.ResponseWriter, req *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		allowRelations(w, req)
	})
	id := decodeBody(t, callAs(r, "user-ok", "POST", "/sessions", `{"name":"Friday"}`))["id"].(string)
	down.Store(true)

	w := callAs(r, "user-ok", "POST", "/sessions/"+id+"/invites", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("invite in an outage = %d %s, want 503", w.Code, w.Body)
	}
}

func TestRejectedTokenAtAuthAPIIs401(t *testing.T) {
	router := sessionRouterWith(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	id := strings.Repeat("a", 24)

	w := callAs(router, "user-ok", "GET", "/sessions/"+id, "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("get = %d %s, want 401", w.Code, w.Body)
	}
}

func TestCreateReturnsTheSessionAtTheMintedID(t *testing.T) {
	const minted = "0123456789abcdef01234567"
	router := sessionRouterWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authz/sessions" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"` + minted + `"}`))
			return
		}
		allowRelations(w, r)
	})

	w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)

	if w.Code != http.StatusCreated || decodeBody(t, w)["id"] != minted {
		t.Fatalf("create = %d %s, want 201 with id %s", w.Code, w.Body, minted)
	}
}

func TestMintRefusalsReachTheClient(t *testing.T) {
	for status, want := range map[int]int{http.StatusForbidden: 403, http.StatusTooManyRequests: 429, http.StatusBadGateway: 503, http.StatusUnauthorized: 401} {
		router := sessionRouterWith(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/authz/sessions" {
				w.WriteHeader(status)
				return
			}
			allowRelations(w, r)
		})

		w := callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)

		if w.Code != want {
			t.Errorf("auth-api %d -> create %d %s, want %d", status, w.Code, w.Body, want)
		}
	}
}

func TestFailedTupleWriteAnswers503MembershipSyncPendingAndTheRetrySucceeds(t *testing.T) {
	var failTuples atomic.Bool
	router := sessionRouterWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authz/session-tuples" && failTuples.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		allowRelations(w, r)
	})
	id := decodeBody(t, callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`))["id"].(string)
	code := inviteCode(t, router, "user-ok", id)
	reqID := decodeBody(t, callAs(router, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`))["id"].(string)
	approve := "/sessions/" + id + "/requests/" + reqID + "/approve"
	failTuples.Store(true)

	w := callAs(router, "user-ok", "POST", approve, `{"role":"player"}`)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "membership_sync_pending") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("approve with tuples failing = %d %s retry-after %q; want 503 membership_sync_pending with Retry-After", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	failTuples.Store(false)
	if w := callAs(router, "user-ok", "POST", approve, `{"role":"player"}`); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", w.Code, w.Body)
	}
}

func TestAuthAPIRefusalsOnAGrantReachTheClient(t *testing.T) {
	for status, want := range map[int]int{http.StatusForbidden: 403, http.StatusTooManyRequests: 429, http.StatusConflict: 409, http.StatusBadRequest: 500} {
		var fail atomic.Bool
		router := sessionRouterWith(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/authz/session-tuples" && fail.Load() {
				w.WriteHeader(status)
				return
			}
			allowRelations(w, r)
		})
		id := decodeBody(t, callAs(router, "user-ok", "POST", "/sessions", `{"name":"Friday"}`))["id"].(string)
		code := inviteCode(t, router, "user-ok", id)
		reqID := decodeBody(t, callAs(router, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`))["id"].(string)
		fail.Store(true)

		w := callAs(router, "user-ok", "POST", "/sessions/"+id+"/requests/"+reqID+"/approve", `{"role":"player"}`)

		if w.Code != want {
			t.Errorf("auth-api %d on a grant -> %d %s, want %d", status, w.Code, w.Body, want)
		}
	}
}
