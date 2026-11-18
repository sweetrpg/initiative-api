package server

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// memberTable is a session run by user-ok (the GM) over an entitlements-api whose
// members_per_session value is limit and which can be switched off. maxMembers, when above zero, is
// the service-configured maximum of the member limit instead of the test default of 20.
type memberTable struct {
	r     http.Handler
	db    *fake.DB
	sid   string
	limit *atomic.Int64
	down  *atomic.Bool
}

func newMemberTable(t *testing.T, limit, maxMembers int) memberTable {
	t.Helper()
	m := memberTable{limit: new(atomic.Int64), down: new(atomic.Bool), db: fake.NewDB()}
	m.limit.Store(int64(limit))
	ent := func(w http.ResponseWriter, req *http.Request) {
		if m.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		v := int64(10)
		if req.URL.Query().Get("key") == domain.KeyMembersPerSession {
			v = m.limit.Load()
		}
		_, _ = w.Write([]byte(`{"key":"` + req.URL.Query().Get("key") + `","type":"limit","value":` + strconv.FormatInt(v, 10) + `,"ttl_seconds":0}`))
	}
	setMax := func(svc *Services) {
		if maxMembers > 0 {
			svc.Sessions.MaxMembers = maxMembers
		}
	}
	m.r, _ = credRouterEnt(t, m.db, roleChecks, ent, setMax)
	w := callAs(m.r, "user-ok", "POST", "/sessions", `{"name":"Friday"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", w.Code, w.Body)
	}
	m.sid = decodeBody(t, w)["id"].(string)
	return m
}

// request has token ask to join and returns the request id.
func (m memberTable) request(t *testing.T, token string) string {
	t.Helper()
	return m.requestVia(t, token, "user-ok")
}

// requestVia is request with the invite made by inviter: a co-GM's invite does not refresh the
// GM's recorded entitlements, which only the GM's own requests do.
func (m memberTable) requestVia(t *testing.T, token, inviter string) string {
	t.Helper()
	code := inviteCode(t, m.r, inviter, m.sid)
	w := callAs(m.r, token, "POST", "/sessions/join", `{"code":"`+code+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%s join = %d %s", token, w.Code, w.Body)
	}
	return decodeBody(t, w)["id"].(string)
}

type httpResult struct {
	code int
	body map[string]any
	raw  string
}

func (m memberTable) approve(t *testing.T, by, reqID, role string) *httpResult {
	t.Helper()
	w := callAs(m.r, by, "POST", "/sessions/"+m.sid+"/requests/"+reqID+"/approve", `{"role":"`+role+`"}`)
	return &httpResult{w.Code, decodeBody(t, w), w.Body.String()}
}

func (m memberTable) pending(t *testing.T) int {
	t.Helper()
	return len(decodeBody(t, callAs(m.r, "user-ok", "GET", "/sessions/"+m.sid+"/requests", ""))["items"].([]any))
}

func (m memberTable) setRecord(t *testing.T, set bson.D) {
	t.Helper()
	var discard data.Session
	if err := m.db.Fake(data.CollectionSessions).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: m.sid}}, set, &discard); err != nil {
		t.Fatal(err)
	}
}

func TestApprovingPastTheMemberLimitIsTheTypedEntitlementErrorAndTheRequestStaysPending(t *testing.T) {
	m := newMemberTable(t, 2, 0)
	a, b, c := m.request(t, "user-two"), m.request(t, "user-three"), m.request(t, "user-four")

	one, two := m.approve(t, "user-ok", a, "player"), m.approve(t, "user-ok", b, "observer")
	three := m.approve(t, "user-ok", c, "player")

	if one.code != 200 || two.code != 200 {
		t.Fatalf("approvals under the limit = %d, %d (%s)", one.code, two.code, two.raw)
	}
	if three.code != http.StatusForbidden || three.body["code"] != "entitlement_limit_reached" || three.body["key"] != domain.KeyMembersPerSession || three.body["limit"] != float64(2) {
		t.Fatalf("approval past the limit = %d %s, want 403 entitlement_limit_reached with the key and limit 2", three.code, three.raw)
	}
	if m.pending(t) != 1 {
		t.Errorf("%d requests pending, want the rejected one to stay pending", m.pending(t))
	}
	m.limit.Store(3)
	if again := m.approve(t, "user-ok", c, "player"); again.code != 200 {
		t.Errorf("approving after the limit rose = %d %s, want 200 for the request left pending", again.code, again.raw)
	}
}

func TestTheGMDoesNotCountTowardTheLimitAndCoGMsDo(t *testing.T) {
	m := newMemberTable(t, 2, 0)
	a, b := m.request(t, "user-four"), m.request(t, "user-two")

	coGM := m.approve(t, "user-ok", a, "co_gm")
	player := m.approve(t, "user-ok", b, "player")

	if coGM.code != 200 || player.code != 200 {
		t.Fatalf("the GM plus a co-GM and a player under a limit of 2 = %d, %d (%s)", coGM.code, player.code, player.raw)
	}
	if third := m.approve(t, "user-ok", m.request(t, "user-three"), "observer"); third.code != http.StatusForbidden {
		t.Fatalf("a third non-GM member = %d %s, want 403: the co-GM and player fill a limit of 2", third.code, third.raw)
	}
}

func TestTheEntitledValueIsClampedToTheConfiguredMaximum(t *testing.T) {
	m := newMemberTable(t, 50, 1)
	a, b := m.request(t, "user-two"), m.request(t, "user-three")

	first := m.approve(t, "user-ok", a, "player")
	second := m.approve(t, "user-ok", b, "player")

	if first.code != 200 || second.code != http.StatusForbidden || second.body["limit"] != float64(1) {
		t.Fatalf("entitled 50, maximum 1: first = %d, second = %d %s; want 200 then 403 carrying limit 1", first.code, second.code, second.raw)
	}
}

func TestALimitThatFallsBelowTheRosterRemovesNobodyAndOnlyStopsAdditions(t *testing.T) {
	m := newMemberTable(t, 5, 0)
	for _, tok := range []string{"user-two", "user-three"} {
		if r := m.approve(t, "user-ok", m.request(t, tok), "player"); r.code != 200 {
			t.Fatalf("approve %s = %d", tok, r.code)
		}
	}
	m.limit.Store(1)

	late := m.approve(t, "user-ok", m.request(t, "user-four"), "player")

	if late.code != http.StatusForbidden {
		t.Fatalf("approval with the limit below the roster = %d %s, want 403", late.code, late.raw)
	}
	for _, tok := range []string{"user-two", "user-three"} {
		if w := callAs(m.r, tok, "GET", "/sessions/"+m.sid, ""); w.Code != 200 {
			t.Errorf("%s lost access when the limit fell: %d", tok, w.Code)
		}
	}
}

func TestARetriedApprovalIsNotBlockedByTheLimitItsOwnMemberFills(t *testing.T) {
	m := newMemberTable(t, 1, 0)
	req := m.request(t, "user-two")
	if first := m.approve(t, "user-ok", req, "player"); first.code != 200 {
		t.Fatalf("approve = %d %s", first.code, first.raw)
	}

	retry := m.approve(t, "user-ok", req, "player")

	if retry.code != 200 {
		t.Fatalf("retried approval at the limit = %d %s, want 200: the member already exists", retry.code, retry.raw)
	}
}

func TestWithNoRecordedLimitAnApprovalFailsClosedWith503(t *testing.T) {
	t.Run("a co-GM approving with no record", func(t *testing.T) {
		m := newMemberTable(t, 5, 0)
		if r := m.approve(t, "user-ok", m.request(t, "user-four"), "co_gm"); r.code != 200 {
			t.Fatalf("setup: %d", r.code)
		}
		m.setRecord(t, bson.D{{Key: "$unset", Value: bson.D{{Key: "owner_limits", Value: ""}, {Key: "owner_limits_at", Value: ""}}}})

		r := m.approve(t, "user-four", m.requestVia(t, "user-two", "user-four"), "player")

		if r.code != http.StatusServiceUnavailable || r.body["code"] != "owner_entitlements_pending" {
			t.Fatalf("= %d %s, want 503 owner_entitlements_pending", r.code, r.raw)
		}
	})
	t.Run("the GM approving while entitlements are down and nothing is recorded", func(t *testing.T) {
		m := newMemberTable(t, 5, 0)
		req := m.request(t, "user-two")
		m.setRecord(t, bson.D{{Key: "$unset", Value: bson.D{{Key: "owner_limits", Value: ""}, {Key: "owner_limits_at", Value: ""}}}})
		m.down.Store(true)

		r := m.approve(t, "user-ok", req, "player")

		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("= %d %s, want 503", r.code, r.raw)
		}
		if m.pending(t) != 1 {
			t.Error("the request should stay pending")
		}
	})
	t.Run("a co-GM relying on a record older than the maximum age", func(t *testing.T) {
		m := newMemberTable(t, 5, 0)
		if r := m.approve(t, "user-ok", m.request(t, "user-four"), "co_gm"); r.code != 200 {
			t.Fatalf("setup: %d", r.code)
		}
		m.setRecord(t, bson.D{{Key: "$set", Value: bson.D{{Key: "owner_limits_at", Value: time.Now().Add(-8 * 24 * time.Hour)}}}})

		r := m.approve(t, "user-four", m.requestVia(t, "user-two", "user-four"), "player")

		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("= %d %s, want 503 for a stale record", r.code, r.raw)
		}
	})
}

func TestACoGMApprovesAgainstTheGMsRecordedLimit(t *testing.T) {
	m := newMemberTable(t, 2, 0)
	if r := m.approve(t, "user-ok", m.request(t, "user-four"), "co_gm"); r.code != 200 {
		t.Fatalf("setup: %d", r.code)
	}

	ok := m.approve(t, "user-four", m.requestVia(t, "user-two", "user-four"), "player")
	full := m.approve(t, "user-four", m.requestVia(t, "user-three", "user-four"), "player")

	if ok.code != 200 || full.code != http.StatusForbidden || full.body["limit"] != float64(2) {
		t.Fatalf("co-GM approvals = %d, %d %s; want 200 then 403 against the recorded limit of 2", ok.code, full.code, full.raw)
	}
}
