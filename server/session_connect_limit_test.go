package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"go.mongodb.org/mongo-driver/bson"
)

// limitTable is a stream table whose session member limit (the entitled 20, clamped by the
// configured maximum) is limit. user-ok is the GM, user-two a player, user-three an observer.
func limitTable(t *testing.T, limit int, opts StreamOptions) streamTable {
	t.Helper()
	st := newStreamTable(t, opts, 0)
	st.svc.Sessions.MaxMembers = limit
	return st
}

// fullTable is limitTable with user-four joined as co-GM first, so the session has three non-GM
// members to connect: user-two, user-three and user-four.
func fullTable(t *testing.T, limit int, opts StreamOptions) streamTable {
	t.Helper()
	st := limitTable(t, 3, opts)
	joinTable(t, st, "user-four", "co_gm")
	st.svc.Sessions.MaxMembers = limit
	return st
}

// refusal dials with a fresh ticket expecting a refusal and returns its status and typed body.
func (st streamTable) refusal(t *testing.T, token, extra string) (int, ErrorBody) {
	t.Helper()
	tk, code := st.ticket(t, token, `{"session_id":"`+st.sid+`"`+extra+`}`)
	if code != http.StatusCreated {
		t.Fatalf("ticket = %d", code)
	}
	c, resp, err := st.dial(t, "ticket="+tk, nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the connect was accepted, want a refusal")
	}
	var body ErrorBody
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &body)
	return resp.StatusCode, body
}

func (st streamTable) accepted(t *testing.T, token, extra string) *websocket.Conn {
	t.Helper()
	c, code := st.open(t, token, extra)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("%s connect = %d, want it accepted", token, code)
	}
	return c
}

func TestAConnectByAnotherMemberAtTheSessionLimitIsRefusedWithTheTypedEntitlementError(t *testing.T) {
	st := fullTable(t, 2, StreamOptions{})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")

	code, body := st.refusal(t, "user-four", "")
	if code != http.StatusForbidden || body.Code != "entitlement_limit_reached" || body.Key != domain.KeyMembersPerSession || body.Limit == nil || *body.Limit != 2 {
		t.Errorf("refusal = %d %+v, want 403 entitlement_limit_reached with the key and limit 2 (the entitled 20 clamped)", code, body)
	}
	if w := callAs(st.r, "user-ok", "GET", "/sessions/"+st.sid+"/members", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "u-2") || !strings.Contains(w.Body.String(), "u-3") {
		t.Errorf("the roster changed: %d %s", w.Code, w.Body)
	}
}

func TestASecondDeviceOfAMemberSharesTheirSeatWhenTheSessionIsFull(t *testing.T) {
	st := fullTable(t, 2, StreamOptions{})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")

	st.accepted(t, "user-two", "") // a second device takes no further seat
	st.accepted(t, "user-three", "")
	if code, _ := st.refusal(t, "user-four", ""); code != http.StatusForbidden {
		t.Errorf("a third member = %d, want 403: the two devices held two members, not four seats", code)
	}
}

func TestAThirdDeviceOfOneMemberIsRefusedWithTheDeviceLimit(t *testing.T) {
	st := fullTable(t, 5, StreamOptions{})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-two", "")

	code, body := st.refusal(t, "user-two", "")
	if code != http.StatusForbidden || body.Code != "device_limit_reached" || body.Key != "devices_per_member" || body.Limit == nil || *body.Limit != 2 {
		t.Errorf("third device = %d %+v, want 403 device_limit_reached with key devices_per_member and limit 2", code, body)
	}
	if code, _ := st.refusal(t, "user-two", `,"client_id":"tab"`); code != http.StatusForbidden {
		t.Errorf("a third device with a client id = %d, want 403", code)
	}
	st.accepted(t, "user-three", "") // another member has devices of their own
}

func TestTheDeviceLimitComesFromTheConfiguredValue(t *testing.T) {
	st := fullTable(t, 5, StreamOptions{DevicesMax: 3})
	for range 3 {
		st.accepted(t, "user-two", "")
	}
	if code, body := st.refusal(t, "user-two", ""); code != http.StatusForbidden || body.Limit == nil || *body.Limit != 3 {
		t.Errorf("a fourth device = %d %+v, want 403 with limit 3", code, body)
	}
}

func TestAClosedDeviceFreesItsPlaceForANewOne(t *testing.T) {
	st := fullTable(t, 5, StreamOptions{})
	a := st.accepted(t, "user-two", "")
	st.accepted(t, "user-two", "")
	_ = a.CloseNow()

	waitFor(t, "the closed device's place to free", func() bool { _, code := st.open(t, "user-two", ""); return code == http.StatusSwitchingProtocols })
}
func TestTheGMConnectsWhenTheSessionIsAtTheLimitButIsHeldToTheDeviceLimit(t *testing.T) {
	st := limitTable(t, 1, StreamOptions{})
	st.accepted(t, "user-two", "")
	if code, _ := st.refusal(t, "user-three", ""); code != http.StatusForbidden {
		t.Fatalf("the session is not full: %d", code)
	}
	st.accepted(t, "user-ok", "") // the GM holds no member seat
	st.accepted(t, "user-ok", "")

	code, body := st.refusal(t, "user-ok", "")
	if code != http.StatusForbidden || body.Code != "device_limit_reached" {
		t.Errorf("the GM's third device = %d %+v, want 403 device_limit_reached", code, body)
	}
}
func TestWhenTheLimitFallsBelowTheMembersConnectedOnlyNewMembersAreCounted(t *testing.T) {
	st := fullTable(t, 3, StreamOptions{})
	a := st.accepted(t, "user-two", "")
	b := st.accepted(t, "user-three", "")

	st.svc.Sessions.MaxMembers = 1 // the owner's limit fell below the 2 connected members

	for _, c := range []*websocket.Conn{a, b} { // a round trip: the server still answers on a connection the limit did not end
		go func() { // a ping's answer is read by the reader, which also takes the frames the server sent
			for {
				if _, _, err := c.Read(t.Context()); err != nil {
					return
				}
			}
		}()
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("a connection was ended because the limit fell: %v", err)
		}
	}
	if n := st.seatsIn("session:"); n != 2 {
		t.Errorf("%d seats after the limit fell, want the 2 connections to keep their seats", n)
	}
	if code, _ := st.refusal(t, "user-four", ""); code != http.StatusForbidden {
		t.Errorf("a new member at 2 connected with limit 1 = %d, want 403", code)
	}
	extra := st.accepted(t, "user-two", "") // a member who holds a seat adds no member
	_ = a.CloseNow()
	_ = b.CloseNow()
	_ = extra.CloseNow()
	waitFor(t, "the closed connections' seats to free", func() bool { return st.seatsIn("session:") == 0 })
	st.accepted(t, "user-four", "") // fewer members connected than the limit again
}
func TestAMemberWhoDropsAndReconnectsInTheGraceKeepsTheirSeatEvenWhenOthersFillTheSession(t *testing.T) {
	st := fullTable(t, 2, StreamOptions{Grace: time.Minute})
	st.accepted(t, "user-two", `,"client_id":"web-a"`)
	dropped := st.accepted(t, "user-three", `,"client_id":"web-b"`)
	_ = dropped.CloseNow()
	waitFor(t, "the dropped seat to be held for the grace", func() bool {
		for _, d := range st.db.Fake(data.CollectionStreamLeases).Docs() {
			if d["dropped"] == true && d["bucket"] != nil && strings.Contains(d["bucket"].(string), "session:") {
				return true
			}
		}
		return false
	})

	if code, _ := st.refusal(t, "user-four", `,"client_id":"web-c"`); code != http.StatusForbidden {
		t.Fatalf("another member during the grace = %d, want 403 (the dropped member's seat still counts)", code)
	}
	st.accepted(t, "user-three", `,"client_id":"web-b"`) // the same client takes its seat back
}
func TestSeatsOfAPodThatCrashedStopCountingAfterTheLeaseTimeout(t *testing.T) {
	st := fullTable(t, 2, StreamOptions{LeaseTTL: 100 * time.Millisecond, Heartbeat: time.Hour})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")
	if code, _ := st.refusal(t, "user-four", ""); code != http.StatusForbidden {
		t.Fatalf("full = %d", code)
	}

	st.advance(150 * time.Millisecond) // nothing renews the seats, as when their pod is gone

	st.accepted(t, "user-four", "")
}
func TestAMemberConnectFailsClosedWhenTheOwnerRecordIsMissingOrStale(t *testing.T) {
	cases := map[string]bson.D{
		"stale":   {{Key: "$set", Value: bson.D{{Key: "owner_limits_at", Value: time.Now().Add(-8 * 24 * time.Hour)}}}},
		"missing": {{Key: "$unset", Value: bson.D{{Key: "owner_limits", Value: ""}, {Key: "owner_limits_at", Value: ""}}}},
	}
	for name, update := range cases {
		t.Run(name, func(t *testing.T) {
			st := limitTable(t, 5, StreamOptions{})
			tk, code := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`) // minted while the record is fresh
			if code != http.StatusCreated {
				t.Fatalf("ticket = %d", code)
			}
			var sess data.Session
			if err := st.db.Fake(data.CollectionSessions).FindOneAndUpdate(t.Context(), bson.D{{Key: "_id", Value: st.sid}}, update, &sess); err != nil {
				t.Fatal(err)
			}

			c, resp, err := st.dial(t, "ticket="+tk, nil)
			if err == nil {
				_ = c.CloseNow()
				t.Fatal("the connect was accepted on an owner record that cannot be trusted")
			}
			var body ErrorBody
			b, _ := io.ReadAll(resp.Body)
			_ = json.Unmarshal(b, &body)
			if resp.StatusCode != http.StatusServiceUnavailable || body.Code != "owner_entitlements_pending" {
				t.Errorf("connect = %d %+v, want 503 owner_entitlements_pending", resp.StatusCode, body)
			}
			if n := len(st.db.Fake(data.CollectionStreamLeases).Docs()); n != 0 {
				t.Errorf("%d seats left behind by the refused connect", n)
			}
		})
	}
}

func TestACoGMHoldsASeatLikeAPlayer(t *testing.T) {
	st := limitTable(t, 3, StreamOptions{})
	joinTable(t, st, "user-four", "co_gm")
	st.svc.Sessions.MaxMembers = 2

	st.accepted(t, "user-four", "")
	st.accepted(t, "user-two", "")
	if code, _ := st.refusal(t, "user-three", ""); code != http.StatusForbidden {
		t.Errorf("a third connection with a co-GM and a player seated = %d, want 403 (the co-GM counts)", code)
	}
}

func TestStreamsOutsideTheSessionRosterTakeNoSessionSeat(t *testing.T) {
	st := limitTable(t, 1, StreamOptions{})
	st.accepted(t, "user-two", "")
	if code, _ := st.refusal(t, "user-three", ""); code != http.StatusForbidden {
		t.Fatalf("the session is full for user-three: %d", code)
	}

	st.openUserLevel(t, "user-three")
	_, secret := issue(t, st.r, "user-three", `["read"]`)
	cred, _, err := st.dial(t, "", http.Header{"Authorization": {"Bearer " + secret}})
	if err != nil {
		t.Fatalf("a credential stream: %v", err)
	}
	t.Cleanup(func() { _ = cred.CloseNow() })
	private, _, err := st.dial(t, "", http.Header{"Authorization": {"Bearer user-three"}})
	if err != nil {
		t.Fatalf("a private (no session) stream: %v", err)
	}
	t.Cleanup(func() { _ = private.CloseNow() })

	for _, d := range st.db.Fake(data.CollectionStreamLeases).Docs() {
		if b, _ := d["bucket"].(string); strings.HasPrefix(b, "session:") && d["user_id"] != "u-2" {
			t.Errorf("a stream outside the roster took a session seat: %v", d)
		}
	}
}

func TestAHeartbeatKeepsASeatPastTheLeaseTimeout(t *testing.T) {
	var renewals atomic.Int64
	st := fullTable(t, 2, StreamOptions{LeaseTTL: 100 * time.Millisecond, Heartbeat: 20 * time.Millisecond, afterRenew: func() { renewals.Add(1) }})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")

	for range 2 { // two lease lifetimes pass on the shared clock, each with its renewals in between
		st.advance(90 * time.Millisecond)
		before := renewals.Load()
		waitFor(t, "both streams to renew", func() bool { return renewals.Load() >= before+6 })
	}

	if code, _ := st.refusal(t, "user-four", ""); code != http.StatusForbidden {
		t.Errorf("a connect after the seats outlived the lease timeout by renewing = %d, want 403 (still full)", code)
	}
}
