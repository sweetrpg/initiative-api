package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

type streamTable struct {
	srv     *httptest.Server
	r       http.Handler
	db      *fake.DB
	sid     string
	hub     *stream.Hub
	streams *domain.Streams
}

func newStreamTable(t *testing.T, opts StreamOptions, ticketTTL time.Duration) streamTable {
	t.Helper()
	var st streamTable
	var captured *fake.DB
	routerHook = func(svc *Services) {
		hub := stream.New(svc.Encounters)
		svc.Encounters.SetSink(hub)
		svc.Sessions.SetSink(hub)
		svc.Sessions.SetEvictor(hub)
		svc.Hub = hub
		svc.Streams = domain.NewStreams(captured, svc.Sessions, svc.Encounters)
		svc.Streams.TTL = ticketTTL
		svc.Stream = opts
		st.hub, st.streams = hub, svc.Streams
	}
	t.Cleanup(func() { routerHook = nil })
	st.r, st.db, st.sid = viewTableOn(t, "3", func(d *fake.DB) store.Database { captured = d; return d })
	seedSystem(t, st.db)
	st.srv = httptest.NewServer(st.r)
	t.Cleanup(st.srv.Close)
	return st
}

// ticket mints a ticket for body as token's user and returns it with the status.
func (st streamTable) ticket(t *testing.T, token, body string) (string, int) {
	t.Helper()
	w := callAs(st.r, token, "POST", "/streams/tickets", body)
	if w.Code != http.StatusCreated {
		return "", w.Code
	}
	return decodeBody(t, w)["ticket"].(string), w.Code
}

func (st streamTable) wsURL(query string) string {
	return "ws" + strings.TrimPrefix(st.srv.URL, "http") + "/streams?" + query
}

func (st streamTable) dial(t *testing.T, query string, h http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return websocket.Dial(ctx, st.wsURL(query), &websocket.DialOptions{HTTPHeader: h})
}

// frame reads one text frame as JSON.
func frame(t *testing.T, c *websocket.Conn) (map[string]any, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("frame %q: %v", b, err)
	}
	return m, string(b)
}

// closeCode reads until the connection closes and returns the close code.
func closeCode(c *websocket.Conn) websocket.StatusCode {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func TestATicketOpensTheStreamWithHelloAndHeads(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, code := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`","client_id":"web-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint = %d", code)
	}
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := frame(t, c)
	if hello["type"] != "hello" || hello["session_id"] != st.sid || hello["client_id"] != "web-1" {
		t.Errorf("hello = %v", hello)
	}
	head, _ := frame(t, c)
	if head["type"] != "head" || head["encounter_id"] != encE1 || head["revision"] == nil {
		t.Errorf("a GM's head = %v, want the revision", head)
	}
}

func TestATicketCannotBeReplayedOrUsedAfterItExpires(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 150*time.Millisecond)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	if _, _, err := st.dial(t, "ticket="+tk, nil); err != nil {
		t.Fatal(err)
	}
	if _, resp, err := st.dial(t, "ticket="+tk, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed ticket: err=%v resp=%v, want 401", err, resp)
	}
	old, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	time.Sleep(250 * time.Millisecond)
	if _, resp, err := st.dial(t, "ticket="+old, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired ticket: err=%v resp=%v, want 401", err, resp)
	}
	if _, resp, err := st.dial(t, "ticket=not-a-ticket", nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown ticket: err=%v, want 401", err)
	}
}

func TestATicketForOneSessionNeverOpensAnother(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	other := decodeBody(t, callAs(st.r, "user-ok", "POST", "/sessions", `{"name":"Saturday"}`))["id"].(string)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk+"&session_id="+other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hello, _ := frame(t, c); hello["session_id"] != st.sid {
		t.Errorf("the stream covers %v, want only the session the ticket was minted for", hello["session_id"])
	}
}

func TestANonMemberCannotMintATicket(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	if _, code := st.ticket(t, "user-four", `{"session_id":"`+st.sid+`"}`); code != http.StatusForbidden {
		t.Errorf("non-member mint = %d, want 403", code)
	}
	if n := len(st.db.Fake("stream_tickets").Docs()); n != 0 {
		t.Errorf("a refused mint stored %d tickets", n)
	}
}

func TestAMemberRemovedBetweenMintAndRedeemGetsNothing(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	if w := callAs(st.r, "user-ok", "DELETE", "/sessions/"+st.sid+"/members/u-2", ""); w.Code >= 300 {
		t.Fatalf("remove = %d", w.Code)
	}
	if _, resp, err := st.dial(t, "ticket="+tk, nil); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("redeem after removal: err=%v, want 403", err)
	}
}

func TestOnlyTheHashOfATicketIsStored(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	for _, d := range st.db.Fake("stream_tickets").Docs() {
		if b, _ := json.Marshal(d); strings.Contains(string(b), tk) {
			t.Errorf("the stored ticket row contains the ticket itself: %s", b)
		}
	}
}

func TestHelloAndHeadsNeverCarryWhatAPlayerMayNotSee(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	for _, token := range []string{"user-two", "user-three"} {
		tk, _ := st.ticket(t, token, `{"session_id":"`+st.sid+`"}`)
		c, _, err := st.dial(t, "ticket="+tk, nil)
		if err != nil {
			t.Fatal(err)
		}
		var all string
		for range 2 {
			_, raw := frame(t, c)
			all += raw
		}
		for _, secret := range []string{`"hid"`, secretName, secretNote, `"revision"`, `"initiative"`} {
			if strings.Contains(all, secret) {
				t.Errorf("%s's hello and head carry %s: %s", token, secret, all)
			}
		}
		if !strings.Contains(all, `"view_tag"`) {
			t.Errorf("%s's head has no view_tag: %s", token, all)
		}
	}
}

func TestAnOriginThatIsNotAllowedIsRefusedBeforeTheTicketIsSpent(t *testing.T) {
	st := newStreamTable(t, StreamOptions{AllowedOrigins: []string{"app.example.test"}}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	bad := http.Header{"Origin": {"https://evil.example.test"}}
	if _, resp, err := st.dial(t, "ticket="+tk, bad); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad origin: err=%v, want 403", err)
	}
	good := http.Header{"Origin": {"https://app.example.test"}}
	if _, _, err := st.dial(t, "ticket="+tk, good); err != nil {
		t.Errorf("the ticket was spent by the refused origin: %v", err)
	}
}

func TestEvictingAMemberEndsTheirStreamWithMembershipChanged(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	if w := callAs(st.r, "user-ok", "DELETE", "/sessions/"+st.sid+"/members/u-2", ""); w.Code >= 300 {
		t.Fatalf("remove = %d", w.Code)
	}
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Errorf("close code = %d, want %d", got, CloseMembershipChanged)
	}
}

func TestAnEvictionOnAnotherPodClosesTheSocketHere(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	col := st.db.Collection(stream.FanoutCollection)
	here := st.hub.AttachRelay(col, "pod-a", 16)
	here.Run(context.Background())
	t.Cleanup(here.Stop)
	other := stream.New(nil)
	there := other.AttachRelay(col, "pod-b", 16)
	there.Run(context.Background())
	t.Cleanup(there.Stop)
	waitFor(t, "the tails", func() bool { return here.Tailing() && there.Tailing() })

	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	other.Evict(st.sid, "u-2")
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Errorf("close code = %d, want %d", got, CloseMembershipChanged)
	}
}

func TestARevokedCredentialClosesItsStreamWith4001(t *testing.T) {
	st := newStreamTable(t, StreamOptions{Heartbeat: 40 * time.Millisecond}, 0)
	id, secret := issue(t, st.r, "user-ok", `["read"]`)
	bearer := http.Header{"Authorization": {"Bearer " + secret}}
	if _, resp, err := st.dial(t, "session_id="+st.sid, bearer); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a credential on a session scope: err=%v, want 403 (session data needs the user's own bearer)", err)
	}
	c, _, err := st.dial(t, "", bearer) // its owner's private encounters
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	if w := callAs(st.r, "user-ok", "POST", "/credentials/"+id+"/revoke", ""); w.Code >= 300 {
		t.Fatalf("revoke = %d", w.Code)
	}
	if got := closeCode(c); got != CloseCredentialRevoked {
		t.Errorf("close code = %d, want %d", got, CloseCredentialRevoked)
	}
}

func TestAStreamEndsAtItsMaximumAge(t *testing.T) {
	st := newStreamTable(t, StreamOptions{MaxAge: 150 * time.Millisecond}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := closeCode(c); got != CloseMaxAge {
		t.Errorf("close code = %d, want %d", got, CloseMaxAge)
	}
}

func TestAHeartbeatArrivesOnTheInterval(t *testing.T) {
	st := newStreamTable(t, StreamOptions{Heartbeat: 40 * time.Millisecond}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if m, _ := frame(t, c); m["type"] == "heartbeat" {
			return
		}
	}
	t.Error("no heartbeat frame")
}

func TestATicketNeverReachesTheAccessLog(t *testing.T) {
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RedactTicket())
	r.Use(sloggin.New(slog.New(slog.NewJSONHandler(&buf, nil))))
	var got string
	r.GET("/streams", func(c *gin.Context) { got = c.GetString(ticketKey); c.Status(http.StatusNoContent) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/streams?ticket=SECRET-TICKET-VALUE&session_id=abc", nil))
	if got != "SECRET-TICKET-VALUE" {
		t.Errorf("the handler lost the ticket: %q", got)
	}
	if strings.Contains(buf.String(), "SECRET-TICKET-VALUE") {
		t.Errorf("the access log contains the ticket: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "session_id=abc") {
		t.Errorf("the rest of the query should still be logged: %s", buf.String())
	}
}

func TestACommittedChangeReachesTheOpenStreamAsAnEventFrame(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	frame(t, c)
	enc := st.encounter(t)
	st.hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: enc, SessionID: st.sid, OwnerID: "u-1", At: time.Now()})
	if ev, _ := frame(t, c); ev["type"] != data.EventUpdated || ev["encounter_id"] != encE1 {
		t.Errorf("event frame = %v", ev)
	}
}

func (st streamTable) encounter(t *testing.T) data.Encounter {
	t.Helper()
	var enc data.Encounter
	if err := st.db.Fake(data.CollectionEncounters).FindOne(context.Background(), bson.D{{Key: "_id", Value: encE1}}, &enc); err != nil {
		t.Fatal(err)
	}
	return enc
}
