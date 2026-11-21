package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

// limitTable is a stream table whose session member limit (the entitled 20, clamped by the
// configured maximum) is limit. user-ok is the GM, user-two a player, user-three an observer.
func limitTable(t *testing.T, limit int, opts StreamOptions) streamTable {
	t.Helper()
	st := newStreamTable(t, opts, 0)
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

func TestAConnectAtTheSessionLimitIsRefusedWithTheTypedEntitlementError(t *testing.T) {
	st := limitTable(t, 2, StreamOptions{})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")

	code, body := st.refusal(t, "user-two", "") // a second device of the same member counts too
	if code != http.StatusForbidden || body.Code != "entitlement_limit_reached" || body.Key != domain.KeyMembersPerSession || body.Limit == nil || *body.Limit != 2 {
		t.Errorf("refusal = %d %+v, want 403 entitlement_limit_reached with the key and limit 2 (the entitled 20 clamped)", code, body)
	}
	if w := callAs(st.r, "user-ok", "GET", "/sessions/"+st.sid+"/members", ""); w.Code != http.StatusOK || !(strings.Contains(w.Body.String(), "u-2") && strings.Contains(w.Body.String(), "u-3")) {
		t.Errorf("the roster changed: %d %s", w.Code, w.Body)
	}
}

func TestTheGMConnectsEvenWhenTheSessionIsAtTheLimit(t *testing.T) {
	st := limitTable(t, 1, StreamOptions{})
	st.accepted(t, "user-two", "")
	if code, _ := st.refusal(t, "user-three", ""); code != http.StatusForbidden {
		t.Fatalf("the session is not full: %d", code)
	}
	st.accepted(t, "user-ok", "")
	st.accepted(t, "user-ok", "") // and as often as the user cap allows: the GM holds no seat
}

func TestWhenTheLimitFallsBelowTheConnectionsOnlyNewOnesAreCounted(t *testing.T) {
	st := limitTable(t, 2, StreamOptions{})
	a := st.accepted(t, "user-two", "")
	b := st.accepted(t, "user-three", "")

	st.svc.Sessions.MaxMembers = 1 // the owner's limit fell below the 2 live connections

	time.Sleep(100 * time.Millisecond)                                    // a connection ended because the limit fell would have released its seat by now
	if n := len(st.db.Fake(data.CollectionStreamLeases).Docs()); n != 2 { // each connection holds one session seat (no per-user cap here)
		t.Errorf("%d seats after the limit fell, want the 2 connections to keep their seats", n)
	}
	if code, _ := st.refusal(t, "user-two", ""); code != http.StatusForbidden {
		t.Errorf("a new connection at 2 live with limit 1 = %d, want 403", code)
	}
	_ = a.CloseNow()
	_ = b.CloseNow()
	waitFor(t, "the closed connections' seats to free", func() bool { return len(st.db.Fake(data.CollectionStreamLeases).Docs()) == 0 })
	st.accepted(t, "user-two", "") // fewer live connections than the limit again
}

func TestAMemberWhoDropsAndReconnectsInTheGraceKeepsTheirSeatEvenWhenOthersFillTheSession(t *testing.T) {
	st := limitTable(t, 2, StreamOptions{Grace: time.Minute})
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

	if code, _ := st.refusal(t, "user-two", `,"client_id":"web-c"`); code != http.StatusForbidden {
		t.Fatalf("another connection during the grace = %d, want 403 (the dropped seat still counts)", code)
	}
	st.accepted(t, "user-three", `,"client_id":"web-b"`) // the same client takes its seat back
}

func TestSeatsOfAPodThatCrashedStopCountingAfterTheLeaseTimeout(t *testing.T) {
	st := limitTable(t, 2, StreamOptions{LeaseTTL: 100 * time.Millisecond, Heartbeat: time.Hour})
	st.accepted(t, "user-two", "")
	st.accepted(t, "user-three", "")
	if code, _ := st.refusal(t, "user-two", ""); code != http.StatusForbidden {
		t.Fatalf("full = %d", code)
	}

	st.advance(150 * time.Millisecond) // nothing renews the seats, as when their pod is gone

	st.accepted(t, "user-two", "")
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
