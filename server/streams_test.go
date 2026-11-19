package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type streamTable struct {
	srv     *httptest.Server
	r       http.Handler
	db      *fake.DB
	sid     string
	hub     *stream.Hub
	streams *domain.Streams
	clock   *atomic.Int64 // unix nanoseconds the tickets are aged by
	down    *atomic.Bool  // auth-api answers 503 while set
	relHits *atomic.Int64 // auth-api checks received while it was down
	svc     *Services
	opts    StreamOptions
}

func (st streamTable) advance(d time.Duration) { st.clock.Add(int64(d)) }

func newStreamTable(t *testing.T, opts StreamOptions, ticketTTL time.Duration) streamTable {
	t.Helper()
	st := streamTable{clock: new(atomic.Int64), down: new(atomic.Bool), relHits: new(atomic.Int64), opts: opts}
	st.clock.Store(time.Now().UnixNano())
	var captured *fake.DB
	routerHook = func(svc *Services) {
		hub := stream.New(svc.Encounters)
		svc.Encounters.SetSink(hub)
		svc.Sessions.SetSink(hub)
		svc.Sessions.SetEvictor(hub)
		svc.Hub = hub
		svc.Streams = domain.NewStreams(captured, svc.Sessions, svc.Encounters)
		svc.Streams.TTL = ticketTTL
		svc.Streams.SetClock(func() time.Time { return time.Unix(0, st.clock.Load()) })
		svc.Stream = opts
		svc.Leases = data.NewStreamLeases(captured)
		svc.Leases.SetClock(func() time.Time { return time.Unix(0, st.clock.Load()) })
		st.hub, st.streams, st.svc = hub, svc.Streams, svc
	}
	t.Cleanup(func() { routerHook = nil })
	st.r, st.db, st.sid = viewTableRel(t, "3", func(d *fake.DB) store.Database { captured = d; return d }, func(w http.ResponseWriter, r *http.Request) {
		if st.down.Load() {
			st.relHits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		roleChecks(w, r)
	})
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

// refused dials expecting the upgrade to fail, and returns the HTTP status it failed with.
func (st streamTable) refused(t *testing.T, query string, h http.Header) int {
	t.Helper()
	c, resp, err := st.dial(t, query, h)
	if err == nil {
		_ = c.CloseNow()
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		t.Fatalf("dial failed without a response: %v", err)
	}
	return resp.StatusCode
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
	st := newStreamTable(t, StreamOptions{}, 30*time.Second)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	if _, _, err := st.dial(t, "ticket="+tk, nil); err != nil {
		t.Fatal(err)
	}
	if got := st.refused(t, "ticket="+tk, nil); got != http.StatusUnauthorized {
		t.Errorf("replayed ticket = %d, want 401", got)
	}
	old, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	st.advance(31 * time.Second)
	if got := st.refused(t, "ticket="+old, nil); got != http.StatusUnauthorized {
		t.Errorf("expired ticket = %d, want 401", got)
	}
	if got := st.refused(t, "ticket=not-a-ticket", nil); got != http.StatusUnauthorized {
		t.Errorf("unknown ticket = %d, want 401", got)
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
	if got := st.refused(t, "ticket="+tk, nil); got != http.StatusForbidden {
		t.Errorf("redeem after removal = %d, want 403", got)
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
	if got := st.refused(t, "ticket="+tk, bad); got != http.StatusForbidden {
		t.Fatalf("bad origin = %d, want 403", got)
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
	if got := st.refused(t, "session_id="+st.sid, bearer); got != http.StatusForbidden {
		t.Errorf("a credential on a session scope = %d, want 403 (session data needs the user's own bearer)", got)
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

func TestAnUnrecoveredPanicNeverPrintsTheTicket(t *testing.T) {
	var out bytes.Buffer
	r := gin.New()
	r.Use(RedactTicket(), gin.RecoveryWithWriter(&out))
	r.GET("/streams", func(c *gin.Context) { panic("boom") })
	w := httptest.NewRecorder()
	secret := "SECRET" + "-TICKET-VALUE" // built here so this file's own source line in a stack trace is not a false hit
	r.ServeHTTP(w, httptest.NewRequest("GET", "/streams?ticket="+secret, nil))
	if w.Code != http.StatusInternalServerError || out.Len() == 0 {
		t.Fatalf("the panic was not recovered and reported: %d", w.Code)
	}
	if strings.Contains(out.String(), secret) {
		t.Errorf("the recovery output contains the ticket: %s", out.String())
	}
}

func TestATicketIsRefusedWhenRedactionIsNotInstalled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := streamHandlers{}
	r.GET("/streams", h.ticketGate)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/streams?ticket=abc", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a ticket with no RedactTicket = %d, want it refused", w.Code)
	}
}

func joinTable(t *testing.T, st streamTable, token, role string) {
	t.Helper()
	joinAs(t, st.r, st.sid, token, role)
}

func TestAMemberPromotedAfterTheMintNeedsANewTicketAndADemotedOneIsServedLower(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	obs, _ := st.ticket(t, "user-three", `{"session_id":"`+st.sid+`"}`)
	if w := callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-3", `{"role":"player"}`); w.Code >= 300 {
		t.Fatalf("promote = %d", w.Code)
	}
	if got := st.refused(t, "ticket="+obs, nil); got != http.StatusForbidden {
		t.Errorf("a ticket minted before a promotion = %d, want 403", got)
	}
	pl, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	if w := callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-2", `{"role":"observer"}`); w.Code >= 300 {
		t.Fatalf("demote = %d", w.Code)
	}
	c, _, err := st.dial(t, "ticket="+pl, nil)
	if err != nil {
		t.Fatalf("a demoted member's ticket should still open, at the lower level: %v", err)
	}
	frame(t, c)
	head, _ := frame(t, c)
	if head["view_tag"] == nil || head["revision"] != nil {
		t.Errorf("a demoted member's head = %v, want the non-GM form", head)
	}
}

func TestRecheckEndsAStreamWhoseLevelChanged(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	v, err := st.streams.View(context.Background(), domain.Claim{UserID: "u-2", SessionID: st.sid, Level: data.ViewPlayer})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.streams.Recheck(context.Background(), v); err != nil {
		t.Fatalf("unchanged access: %v", err)
	}
	callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-2", `{"role":"observer"}`)
	if err := st.streams.Recheck(context.Background(), v); !errors.Is(err, domain.ErrAccessChanged) {
		t.Errorf("after a demotion Recheck = %v, want ErrAccessChanged", err)
	}
	deleteSession(st.r, "user-ok", st.sid, sessionRevision(t, st.r, st.sid))
	if err := st.streams.Recheck(context.Background(), v); !errors.Is(err, domain.ErrAccessChanged) {
		t.Errorf("after the session was deleted Recheck = %v, want ErrAccessChanged", err)
	}
}

func TestATicketForADeletedSessionOpensNothing(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	if w := deleteSession(st.r, "user-ok", st.sid, sessionRevision(t, st.r, st.sid)); w.Code >= 300 {
		t.Fatalf("delete = %d", w.Code)
	}
	if got := st.refused(t, "ticket="+tk, nil); got != http.StatusForbidden {
		t.Errorf("a ticket for a deleted session = %d, want 403", got)
	}
}

func TestAnotherUsersPrivateEncounterGetsNoTicket(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	w := callAs(st.r, "user-ok", "POST", "/encounters", `{"name":"Mine","game_system_id":"`+data.DefaultSystemID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	id := decodeBody(t, w)["id"].(string)
	want := callAs(st.r, "user-two", "GET", "/encounters/"+id, "").Code // what the REST read answers (PADR-0035)
	if _, code := st.ticket(t, "user-two", `{"encounter_id":"`+id+`"}`); code != want || want < 400 {
		t.Errorf("another user's private encounter mint = %d, want what the REST read answers (%d)", code, want)
	}
	if _, code := st.ticket(t, "user-ok", `{"encounter_id":"`+id+`"}`); code != http.StatusCreated {
		t.Errorf("the owner's mint = %d", code)
	}
	if _, code := st.ticket(t, "user-ok", `{"encounter_id":"`+encE1+`"}`); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("a session encounter without its session_id = %d, want 403 or 404", code)
	}
}

func TestACoGMStreamsAtTheGMLevel(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	joinTable(t, st, "user-four", "co_gm")
	tk, code := st.ticket(t, "user-four", `{"session_id":"`+st.sid+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("co-GM mint = %d", code)
	}
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	if head, _ := frame(t, c); head["revision"] == nil {
		t.Errorf("a co-GM's head = %v, want the revision", head)
	}
}

func TestABearerUpgradeFollowsTheRESTAuthorization(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	if got := st.refused(t, "session_id="+st.sid, http.Header{"Authorization": {"Bearer user-four"}}); got != http.StatusForbidden {
		t.Errorf("a non-member's bearer upgrade = %d, want 403", got)
	}
	st.down.Store(true)
	if got := st.refused(t, "session_id="+st.sid, http.Header{"Authorization": {"Bearer user-ok"}}); got != http.StatusServiceUnavailable {
		t.Errorf("a bearer upgrade with auth-api down = %d, want 503 (fail closed)", got)
	}
	if st.relHits.Load() == 0 {
		t.Error("the outage was never reached: the stub was not asked while down")
	}
}

func TestAHeadsListPastTheLimitIsReportedAsTruncated(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	for i := 0; i < domain.MaxHeads+1; i++ {
		seedEncounter(t, st.db, primitive.NewObjectID().Hex(), st.sid)
	}
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hello, _ := frame(t, c); hello["heads_truncated"] != true {
		t.Errorf("hello = %v, want heads_truncated", hello)
	}
}

func TestABadClientIDOnTheBearerPathIsIgnored(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	c, _, err := st.dial(t, "client_id=bad%20id%3Cscript%3E", http.Header{"Authorization": {"Bearer user-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if hello, _ := frame(t, c); hello["client_id"] != "" {
		t.Errorf("hello client_id = %v, want the invalid id dropped", hello["client_id"])
	}
}

func TestAnEmptyOrDuplicatedTicketIsRefusedAndNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	old := accessLogger
	accessLogger = slog.New(slog.NewJSONHandler(&logs, nil))
	t.Cleanup(func() { accessLogger = old })
	st := newStreamTable(t, StreamOptions{}, 0)
	bogus := "bogus" + "-ticket-value"
	for _, q := range []string{"ticket=", "ticket=&ticket=" + bogus, "ticket=" + bogus + "&ticket="} {
		w := httptest.NewRecorder()
		st.r.ServeHTTP(w, httptest.NewRequest("GET", "/streams?"+q, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET /streams?%s = %d, want 401", q, w.Code)
		}
	}
	real, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	if _, _, err := st.dial(t, "ticket="+real, nil); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{bogus, real} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the access log of the real router chain contains a ticket: %s", secret)
		}
	}
	if !strings.Contains(logs.String(), "/streams") {
		t.Error("the access log recorded no stream request, so the check proved nothing")
	}
}

func TestAnEvictionBetweenOpenAndGrantIsRefusedAs403(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	h := streamHandlers{svc: *st.svc, opts: st.opts}
	r := gin.New()
	r.Use(typedErrors())
	r.GET("/s", func(c *gin.Context) {
		h.serve(c, serveArgs{actor: "u-2", session: st.sid, authorize: func(context.Context) (data.Viewer, error) {
			st.hub.Evict(st.sid, "u-2") // the member is removed while authorization is in flight
			return st.streams.View(context.Background(), domain.Claim{UserID: "u-2", SessionID: st.sid, Level: data.ViewPlayer})
		}})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/s", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("evicted between Open and Grant = %d, want 403", w.Code)
	}
}

func TestACredentialIsClosedAfterThreeFailedLivenessReadsInARow(t *testing.T) {
	var live bool
	var err error
	guard := liveGuard(func(context.Context) (bool, error) { return live, err })
	ctx := context.Background()
	err = errors.New("store down")
	for i := 1; i <= 2; i++ {
		if got := guard(ctx); got != nil {
			t.Fatalf("failed read %d closed the stream; want it kept until the third", i)
		}
	}
	err, live = nil, true
	if got := guard(ctx); got != nil { // a success resets the count
		t.Fatalf("a healthy read = %v", got)
	}
	err = errors.New("store down")
	for i := 1; i <= 2; i++ {
		if got := guard(ctx); got != nil {
			t.Fatalf("after a reset, failed read %d closed the stream", i)
		}
	}
	if got := guard(ctx); !errors.Is(got, errCredentialRevoked) {
		t.Errorf("third failed read in a row = %v, want revoked", got)
	}
	err, live = nil, false
	if got := guard(ctx); !errors.Is(got, errCredentialRevoked) {
		t.Errorf("a revoked credential = %v, want revoked", got)
	}
}

// hold makes the connection's writer wait at the first event it takes until release is called, so
// a test decides exactly how many events are buffered behind it.
func hold() (opt func(*StreamOptions), release func(), taken chan struct{}) {
	gate, taken := make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	opt = func(o *StreamOptions) {
		o.beforeWrite = func() {
			select {
			case taken <- struct{}{}:
			default:
			}
			<-gate
		}
	}
	return opt, func() { once.Do(func() { close(gate) }) }, taken
}

func TestAClientThatFallsBehindIsClosedWith4003(t *testing.T) {
	opt, release, taken := hold()
	o := StreamOptions{Buffer: 1}
	opt(&o)
	st := newStreamTable(t, o, 0)
	t.Cleanup(release)
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	frame(t, c)
	enc := st.encounter(t)
	emit := func(name string) {
		enc.Name = name // a different visible view each time, so each change is sent
		st.hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: enc, SessionID: st.sid, OwnerID: "u-1", At: time.Now()})
	}
	emit("one")
	<-taken // the writer holds event one; the buffer (1) is empty
	emit("two")
	emit("three") // the buffer is full: the subscriber is closed
	release()
	if got := closeCode(c); got != CloseSlowConsumer {
		t.Errorf("close code = %d, want %d", got, CloseSlowConsumer)
	}
}

func TestAnEvictedStreamWritesNoneOfItsBufferedEvents(t *testing.T) {
	opt, release, taken := hold()
	o := StreamOptions{}
	opt(&o)
	st := newStreamTable(t, o, 0)
	t.Cleanup(release)
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	frame(t, c)
	enc := st.encounter(t)
	for i, name := range []string{"one", "two", "three"} {
		enc.Name = name
		st.hub.Emit(context.Background(), data.Change{Type: data.EventUpdated, After: enc, SessionID: st.sid, OwnerID: "u-1", At: time.Now()})
		if i == 0 {
			<-taken
		}
	}
	st.hub.Evict(st.sid, "u-2")
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != CloseMembershipChanged {
				t.Errorf("close code = %d, want %d", got, CloseMembershipChanged)
			}
			return
		}
		t.Errorf("an evicted stream still wrote a frame: %s", b)
	}
}

func TestAClientDataMessageEndsTheStream(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, bytes.Repeat([]byte("x"), 4096))
	// CloseRead refuses any data message (1008) before the 1 KiB read limit could matter; the limit
	// stays as the bound should that ever change.
	if got := closeCode(c); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %d, want 1008", got)
	}
}

func TestADemotionWithNoEvictionIsFoundByTheNextHeartbeat(t *testing.T) {
	st := newStreamTable(t, StreamOptions{Heartbeat: 40 * time.Millisecond}, 0)
	st.svc.Sessions.SetEvictor(nil) // only the heartbeat's re-check can notice
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	if w := callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-2", `{"role":"observer"}`); w.Code >= 300 {
		t.Fatalf("demote = %d", w.Code)
	}
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Errorf("close code = %d, want %d", got, CloseMembershipChanged)
	}
}

func TestADemotedMembersTicketServesTheLowerLevel(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-2", `{"role":"observer"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame(t, c)
	head, _ := frame(t, c)
	want := strings.Trim(callAs(st.r, "user-three", "GET", "/sessions/"+st.sid+"/encounters/"+encE1, "").Header().Get("ETag"), `"`)
	if head["view_tag"] != want || want == "" {
		t.Errorf("head view_tag = %v, want the observer's %q", head["view_tag"], want)
	}
}

func TestARequestPastItsDeadlineIs503TimeoutButTheStreamUpgradeIsExempt(t *testing.T) {
	r := gin.New()
	r.Use(RequestDeadline(30 * time.Millisecond))
	slow := func(c *gin.Context) {
		<-c.Request.Context().Done()
		writeError(c, c.Request.Context().Err(), nil)
	}
	r.GET("/slow", slow)
	var streamDeadline bool
	r.GET("/streams", func(c *gin.Context) {
		_, streamDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusNoContent)
	})
	var mintDeadline bool
	r.POST("/streams/tickets", func(c *gin.Context) { _, mintDeadline = c.Request.Context().Deadline(); c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/slow", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"timeout"`) || w.Header().Get("Retry-After") == "" {
		t.Errorf("a request past its deadline = %d %s retry-after %q, want 503 timeout", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/streams", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/streams/tickets", nil))
	if streamDeadline {
		t.Error("the stream upgrade has a request deadline; a long-lived connection must not")
	}
	if !mintDeadline {
		t.Error("minting a ticket has no deadline")
	}
}

// open connects a GM stream with a fresh ticket and reads its hello.
func (st streamTable) open(t *testing.T, token, extra string) (*websocket.Conn, int) {
	t.Helper()
	tk, _ := st.ticket(t, token, `{"session_id":"`+st.sid+`"`+extra+`}`)
	c, resp, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		if resp == nil {
			t.Fatalf("dial failed without a response: %v", err)
		}
		return nil, resp.StatusCode
	}
	frame(t, c)
	return c, http.StatusSwitchingProtocols
}

func TestAUserCannotHoldMoreStreamsThanTheCapAndASeatFreesWhenAStreamCloses(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 2}, 0)
	a, _ := st.open(t, "user-ok", "")
	st.open(t, "user-ok", "")
	if _, code := st.open(t, "user-ok", ""); code != http.StatusTooManyRequests {
		t.Fatalf("third stream = %d, want 429", code)
	}
	if n := len(st.db.Fake(data.CollectionStreamLeases).Docs()); n != 2 {
		t.Errorf("the refused stream left a seat behind: %d rows, want 2", n)
	}
	if _, code := st.open(t, "user-two", ""); code != http.StatusSwitchingProtocols {
		t.Errorf("another user's stream = %d, want it unaffected by the cap", code)
	}
	_ = a.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the seat to free", func() bool { _, code := st.open(t, "user-ok", ""); return code == http.StatusSwitchingProtocols })
}

func TestADroppedClientKeepsItsSeatForTheGraceAndOthersCannotTakeIt(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, Grace: time.Minute}, 0)
	c, _ := st.open(t, "user-ok", `,"client_id":"web-1"`)
	_ = c.CloseNow()
	waitFor(t, "the seat to be marked dropped", func() bool {
		for _, d := range st.db.Fake(data.CollectionStreamLeases).Docs() {
			if d["dropped"] == true {
				return true
			}
		}
		return false
	})
	if _, code := st.open(t, "user-ok", `,"client_id":"web-2"`); code != http.StatusTooManyRequests {
		t.Errorf("another client during the grace = %d, want 429", code)
	}
	if _, code := st.open(t, "user-ok", `,"client_id":"web-1"`); code != http.StatusSwitchingProtocols {
		t.Errorf("the same client reconnecting = %d, want it to keep its seat", code)
	}
}

func TestASeatThatIsNotRenewedLapses(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, LeaseTTL: 100 * time.Millisecond, Heartbeat: time.Hour}, 0)
	st.open(t, "user-ok", "") // a pod that crashed here would leave this seat behind
	if _, code := st.open(t, "user-ok", ""); code != http.StatusTooManyRequests {
		t.Fatalf("second stream = %d, want 429 while the seat is live", code)
	}
	st.advance(150 * time.Millisecond)
	if _, code := st.open(t, "user-ok", ""); code != http.StatusSwitchingProtocols {
		t.Errorf("after the lease lapsed = %d, want a free seat", code)
	}
}

func TestAFullPodAnswersOnTheSocketWith1013(t *testing.T) {
	st := newStreamTable(t, StreamOptions{MaxPerPod: 1}, 0)
	st.open(t, "user-ok", "")
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatalf("a full pod should accept the upgrade and say so: %v", err)
	}
	if got := closeCode(c); got != websocket.StatusTryAgainLater {
		t.Errorf("close code = %d, want 1013", got)
	}
}

// ticketStillWorks mints nothing: it dials with tk and reports whether the upgrade succeeded.
func (st streamTable) ticketStillWorks(t *testing.T, tk string) bool {
	t.Helper()
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err == nil {
		_ = c.CloseNow()
	}
	return err == nil
}

func TestARefusedConnectLeavesTheTicketRedeemable(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, MaxPerPod: 2}, 0)
	st.open(t, "user-ok", "")
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	if got := st.refused(t, "ticket="+tk, nil); got != http.StatusTooManyRequests {
		t.Fatalf("second stream = %d, want 429", got)
	}
	// The cap is still full, so the ticket is not spent on the next try either; free the seat and use it.
	for _, d := range st.db.Fake(data.CollectionStreamLeases).Docs() {
		_ = st.db.Fake(data.CollectionStreamLeases).DeleteOne(context.Background(), bson.D{{Key: "_id", Value: d["_id"]}})
	}
	if !st.ticketStillWorks(t, tk) {
		t.Error("a 429 spent the ticket")
	}
}

func TestAFullPodDoesNotSpendTheTicketAndCountsNothing(t *testing.T) {
	st := newStreamTable(t, StreamOptions{MaxPerPod: 1, ConcurrentMax: 5}, 0)
	first, _ := st.open(t, "user-ok", "")
	tk, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := closeCode(c); got != websocket.StatusTryAgainLater {
		t.Fatalf("close code = %d, want 1013", got)
	}
	_ = first.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the slot to free", func() bool { return st.ticketStillWorks(t, tk) })
}

func TestThePodSlotCounterNeverCountsARefusedAttempt(t *testing.T) {
	h := streamHandlers{opts: StreamOptions{MaxPerPod: 2}, open: new(atomic.Int64)}
	r1, ok1 := h.admit()
	r2, ok2 := h.admit()
	_, ok3 := h.admit()
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("admit = %v %v %v, want true true false", ok1, ok2, ok3)
	}
	if n := h.open.Load(); n != 2 {
		t.Errorf("open = %d after a refused attempt, want 2", n)
	}
	r1()
	r2()
	if n := h.open.Load(); n != 0 {
		t.Errorf("open = %d after both released, want 0", n)
	}
}

func TestTheSlotIsReleasedWhenARealHandlerPanicsOrFailsToAuthorize(t *testing.T) {
	st := newStreamTable(t, StreamOptions{MaxPerPod: 1}, 0)
	for name, hook := range map[string]func(){"panic": func() { panic("boom") }, "deny": nil} {
		opts := st.opts
		opts.onServe = hook
		h := streamHandlers{svc: *st.svc, opts: opts, open: new(atomic.Int64)}
		r := gin.New()
		r.Use(gin.Recovery(), func(c *gin.Context) { c.Set(authzViewerKey, "u-9"); c.Next() })
		r.GET("/streams", h.bearerStream) // a non-member of the session: authorization refuses it
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/streams?session_id="+st.sid, nil))
		if n := h.open.Load(); n != 0 {
			t.Errorf("%s: %d slots still held (status %d)", name, n, w.Code)
		}
	}
}

func TestAJunkTicketOnAFullPodIsAPlain401WithNoUpgrade(t *testing.T) {
	st := newStreamTable(t, StreamOptions{MaxPerPod: 1}, 0)
	first, _ := st.open(t, "user-ok", "")
	defer func() { _ = first.CloseNow() }()
	if got := st.refused(t, "ticket=not-a-real-ticket", nil); got != http.StatusUnauthorized {
		t.Errorf("a junk ticket on a full pod = %d, want 401 and no upgrade", got)
	}
}

func TestRefusalsInFlightAreBounded(t *testing.T) {
	h := streamHandlers{opts: StreamOptions{MaxPerPod: 1}, open: new(atomic.Int64), refusing: make(chan struct{}, 1)}
	h.refusing <- struct{}{} // the one allowed refusal is already in flight
	r := gin.New()
	r.GET("/", h.refuseFull)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("past the bound: status %d, Retry-After %q; want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestConcurrentConnectsWithOneTicketOpenOneStreamAndHoldOneSeat(t *testing.T) {
	// The cap is above the number of connects, so the only refusal left is the lost ticket race
	// (each connect takes a seat before it redeems, so a lower cap could refuse them all).
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 20}, 0)
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	const n = 12
	var wg sync.WaitGroup
	var won atomic.Int32
	conns := make(chan *websocket.Conn, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// dial only returns the error; nothing here calls t.Fatal off the test goroutine.
			if c, _, err := st.dial(t, "ticket="+tk, nil); err == nil {
				won.Add(1)
				conns <- c
			}
		}()
	}
	wg.Wait()
	close(conns)
	for c := range conns {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	if won.Load() != 1 {
		t.Errorf("%d connects opened a stream, want exactly 1", won.Load())
	}
	waitFor(t, "the losers' seats to be given back", func() bool { return len(st.db.Fake(data.CollectionStreamLeases).Docs()) == 1 })
}

func TestACredentialStreamUsesItsOwnBucketNotItsOwners(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, CredentialMax: 1}, 0)
	_, secret := issue(t, st.r, "user-ok", `["read"]`)
	bearer := http.Header{"Authorization": {"Bearer " + secret}}
	st.open(t, "user-ok", "") // the owner's one seat
	c, _, err := st.dial(t, "", bearer)
	if err != nil {
		t.Fatalf("a credential stream while its owner holds a full cap: %v", err)
	}
	_ = c
	if got := st.refused(t, "", bearer); got != http.StatusTooManyRequests {
		t.Errorf("a second stream on the same credential = %d, want 429 from its own cap", got)
	}
	if _, code := st.open(t, "user-ok", ""); code != http.StatusTooManyRequests {
		t.Errorf("the owner's second stream = %d, want 429", code)
	}
}

func TestTooManyStreamsCarriesRetryAfter(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1}, 0)
	st.open(t, "user-ok", "")
	_, resp, err := st.dial(t, "", http.Header{"Authorization": {"Bearer user-ok"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("err=%v resp=%v, want 429 with Retry-After", err, resp)
	}
}

func TestASeatStoreOutageIs503AndLeavesTheTicketRedeemable(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 5}, 0)
	good := st.svc.Leases
	st.svc.Leases = data.NewStreamLeases(brokenSeats{st.db})
	tk, _ := st.ticket(t, "user-ok", `{"session_id":"`+st.sid+`"}`)
	h := streamHandlers{svc: *st.svc, opts: st.opts, open: new(atomic.Int64)}
	r := gin.New()
	r.Use(RedactTicket(), typedErrors())
	r.GET("/streams", h.ticketGate)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/streams?ticket="+tk, nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("seat outage = %d retry-after %q, want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	st.svc.Leases = good
	if !st.ticketStillWorks(t, tk) {
		t.Error("a seat outage spent the ticket")
	}
}

type brokenSeats struct{ *fake.DB }

func (b brokenSeats) Collection(name string) store.Collection {
	return brokenInsert{b.Fake(name)}
}

type brokenInsert struct{ *fake.Collection }

func (brokenInsert) InsertOne(context.Context, any) error { return errors.New("mongo down") }

func TestRenewalsKeepAnOpenStreamsSeatAlive(t *testing.T) {
	renewed := make(chan struct{}, 64)
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, LeaseTTL: 100 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
		afterRenew: func() {
			select {
			case renewed <- struct{}{}:
			default:
			}
		}}, 0)
	c, _ := st.open(t, "user-ok", "")
	defer func() { _ = c.CloseNow() }()
	for range 6 {
		st.advance(80 * time.Millisecond) // each step is within one lease TTL of the last renewal
		select {
		case <-renewed: // a renewal after this step
		case <-time.After(5 * time.Second):
			t.Fatal("no renewal happened")
		}
		for len(renewed) > 0 { // wait for one that ran after the advance, not an earlier one
			<-renewed
		}
		select {
		case <-renewed:
		case <-time.After(5 * time.Second):
			t.Fatal("no renewal happened")
		}
	}
	if _, code := st.open(t, "user-ok", ""); code != http.StatusTooManyRequests {
		t.Errorf("after 480ms of renewals the seat should still be held; second stream = %d", code)
	}
}

func TestAServerEndedStreamFreesItsSeatInsteadOfKeepingGrace(t *testing.T) {
	st := newStreamTable(t, StreamOptions{ConcurrentMax: 1, Grace: time.Minute}, 0)
	c, _ := st.open(t, "user-two", `,"client_id":"web-1"`)
	st.hub.Evict(st.sid, "u-2")
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Fatalf("close code = %d", got)
	}
	waitFor(t, "the seat to be freed", func() bool { return len(st.db.Fake(data.CollectionStreamLeases).Docs()) == 0 })
}
