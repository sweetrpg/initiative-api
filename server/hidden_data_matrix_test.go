package server

// Openspec 7.14: players and observers never receive hidden data. One matrix over every surface
// that carries encounter data to them: each REST route (found by walking the router, so a new GET
// route fails this test until it is classified), the WebSocket frames, the addressed notices on a
// session stream and on the user-level stream, and the bus envelope. The fixture's hidden
// adversary "hid" (Assassin Vex) is the GM's secret; its note, initiative and health are GM-only.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/events/stream"
)

// everything a viewer who may not see the hidden combatant must never read.
var hiddenSecrets = []string{secretName, secretNote, `"hid"`, "GM only orc note", `"initiative"`, `"health"`, `"current":31`, `"revision"`, `"owner_user_id"`, "updated_at"}

// restHidden is what a REST body for a member who cannot see the hidden combatant must not hold. A
// visible combatant's own health and a session's own revision are legitimate in a REST body, so the
// hidden one's value (31) stands for its health; encounter bodies also carry no revision, owner or
// timestamps (their consumers order by view_tag).
var restHidden = []string{secretName, secretNote, `"hid"`, "GM only orc note", `"initiative"`, `"current":31`}
var encounterHidden = append(append([]string{}, restHidden...), `"revision"`, `"owner_user_id"`, "updated_at")

// what even the hidden combatant's own controller must never read (its id and name are theirs).
var gmOnlySecrets = []string{secretNote, "GM only orc note", `"initiative"`, `"health"`, `"current":31`, `"revision"`, `"view_tag"`, `"owner_user_id"`, "updated_at"}

var gmOnlySecretsNoTag = func() []string {
	var out []string
	for _, s := range gmOnlySecrets {
		if s != `"view_tag"` {
			out = append(out, s)
		}
	}
	return out
}()

func leaks(body string, secrets []string) []string {
	var out []string
	for _, s := range secrets {
		if strings.Contains(body, s) {
			out = append(out, s)
		}
	}
	return out
}

// restRoutes are the GET routes that carry a session's or an encounter's data to a member; the
// rest are other resources, none of which holds encounter data.
func restRoutes(path string) (carriesEncounterData bool) {
	return strings.HasPrefix(path, "/sessions")
}

var nonEncounterGETs = map[string]bool{
	"/status/ping": true, "/status/health": true, "/themes": true, "/themes/packaged": true, "/themes/:id": true,
	"/encounters": true, "/encounters/:id": true, // the caller's own private encounters, owner-scoped
	"/entitlements": true, "/rosters": true, "/rosters/:id": true, "/game-systems": true, "/game-systems/:id": true,
	"/credentials": true, "/combatants": true, "/admin/credentials": true, "/admin/credentials/deletions": true, "/probe": true,
}

func TestEveryRESTRouteThatCarriesSessionDataHidesTheHiddenCombatantFromPlayersAndObservers(t *testing.T) {
	r, _, sid := viewTable(t)
	engine := r.(*gin.Engine)
	fill := strings.NewReplacer(":id", sid, ":eid", "5f00000000000000000000e1")
	checked := 0
	for _, rt := range engine.Routes() {
		if rt.Method != http.MethodGet {
			continue
		}
		if !restRoutes(rt.Path) {
			if !nonEncounterGETs[rt.Path] {
				t.Errorf("GET %s is new: classify it as carrying session data (and so covered here) or as another resource", rt.Path)
			}
			continue
		}
		for _, cid := range []string{"pc", "hid", "orc"} {
			path := fill.Replace(strings.ReplaceAll(rt.Path, ":cid", cid))
			for _, token := range []string{"user-two", "user-three"} {
				w := callAs(r, token, "GET", path, "")
				checked++
				if w.Code >= 500 {
					t.Errorf("%s GET %s = %d", token, path, w.Code)
				}
				secrets := restHidden
				if strings.Contains(path, "/encounters") {
					secrets = encounterHidden
				}
				if l := leaks(w.Body.String(), secrets); len(l) > 0 {
					t.Errorf("%s GET %s = %d leaks %v: %s", token, path, w.Code, l, w.Body)
				}
				if cid == "hid" && strings.Contains(rt.Path, ":cid") && w.Code != http.StatusNotFound {
					t.Errorf("%s GET %s = %d, want 404 for a combatant they cannot see", token, path, w.Code)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no REST route was exercised")
	}
}

// hiddenControlledBy gives the hidden adversary to u-2, so a notice about it is addressed to a
// player who may see it, and makes it the current combatant.
func hiddenControlledBy(enc data.Encounter, user string) data.Encounter {
	cs := append([]data.Combatant(nil), enc.Combatants...)
	for i := range cs {
		if cs[i].ID == "hid" {
			cs[i].ControllerUserIDs = []string{user}
		}
	}
	enc.Combatants = cs
	enc.CurrentCombatantID, enc.TurnQueue = "hid", []string{"hid", "pc", "orc"}
	return enc
}

func TestAddressedNoticesAboutAHiddenCombatantReachOnlyItsControllerAndCarryNoGMOnlyData(t *testing.T) {
	n := newNoticeTable(t)
	gm := subscribe(n.hub, n.viewer(t, "user-ok"), encE1, 16)
	controller := subscribe(n.hub, n.viewer(t, "user-two"), encE1, 16)
	observer := subscribe(n.hub, n.viewer(t, "user-three"), encE1, 16)
	controllerUserLevel := n.hub.OpenUser("u-2", 16)
	observerUserLevel := n.hub.OpenUser("u-3", 16)
	n.hub.SetMembers(n.ses)

	enc := hiddenControlledBy(n.encounter(t), "u-2")
	n.hub.Emit(context.Background(), data.Change{Type: data.EventTurn, After: enc, SessionID: n.sid, OwnerID: "u-1", At: time.Now()})
	n.hub.Flush()

	if got := notices(drain(gm)); len(got) != 0 {
		t.Errorf("the GM, who controls nothing, got notices %+v", got)
	}
	if got := notices(drain(observer)); len(got) != 0 {
		t.Errorf("an observer got a notice about a hidden combatant: %+v", got)
	}
	if got := drain(observerUserLevel); len(got) != 0 {
		t.Errorf("the observer's user-level stream got %+v", got)
	}
	for name, sub := range map[string]*stream.Subscription{"session stream": controller, "user-level stream": controllerUserLevel} {
		var got []stream.Event
		for _, e := range drain(sub) {
			if len(notices([]stream.Event{e})) == 1 {
				got = append(got, e)
			}
		}
		if len(got) == 0 {
			t.Fatalf("the controller's %s got no notice for its combatant", name)
		}
		for _, e := range got {
			if l := leaks(js(e), gmOnlySecrets); len(l) > 0 {
				t.Errorf("the controller's %s notice leaks %v: %s", name, l, js(e))
			}
		}
	}
}

func TestTheBusEnvelopeCarriesNoHiddenDataForStateEventsOrNotices(t *testing.T) {
	n := newNoticeTable(t)
	bus := &captureBus{}
	n.hub.AttachBus(bus, 256)
	t.Cleanup(n.hub.Close)
	enc := hiddenControlledBy(n.encounter(t), "u-2")

	for _, typ := range allEventTypes {
		for _, ids := range [][]string{{"hid"}, {"hid", "orc"}, {"orc"}, nil} {
			n.hub.Emit(context.Background(), data.Change{Type: typ, After: enc, SessionID: n.sid, OwnerID: "u-1", CombatantID: ids, Origin: data.Origin{Kind: data.OriginUser, ClientID: "ios-7", ActorID: "u-1"}, At: time.Now()})
		}
	}
	n.hub.Flush()
	eventually(t, func() bool { return bus.count() >= len(allEventTypes) }, "the bus never got the events")
	time.Sleep(100 * time.Millisecond)

	bus.mu.Lock()
	defer bus.mu.Unlock()
	var sawNotice bool
	for _, ev := range bus.events {
		body := js(ev)
		if ev.UserID == "" { // a state event: any consumer may read it, so it is the observer's view
			if l := leaks(body, append(hiddenSecrets, "ios-7")); len(l) > 0 {
				t.Errorf("a bus state event %s leaks %v: %s", ev.Type, l, body)
			}
			continue
		}
		sawNotice = true
		if ev.UserID != "u-2" {
			t.Errorf("a notice about a hidden combatant was addressed to %s: %s", ev.UserID, body)
		}
		if l := leaks(body, append(gmOnlySecrets, "ios-7")); len(l) > 0 {
			t.Errorf("a bus notice leaks %v: %s", l, body)
		}
	}
	if !sawNotice {
		t.Error("no addressed notice reached the bus, so the notice envelope was not exercised")
	}
}

// readAll reads frames until none arrives for quiet; the read timeout ends the connection.
func readAll(t *testing.T, c *websocket.Conn, quiet time.Duration) []string {
	t.Helper()
	var out []string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), quiet)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, string(b))
	}
}

func TestWebSocketFramesToPlayersAndObserversCarryNoHiddenData(t *testing.T) {
	st := newStreamTable(t, StreamOptions{Heartbeat: time.Hour}, 0)
	player, _ := st.open(t, "user-two", "")
	observer, _ := st.open(t, "user-three", "")
	userLevel, _ := st.openUserLevel(t, "user-three")
	enc := hiddenControlledBy(userStreamEncounter(t, st), "u-2")

	for _, typ := range allEventTypes {
		for _, ids := range [][]string{{"hid"}, {"hid", "orc"}, {"orc"}, nil} {
			st.hub.Emit(context.Background(), data.Change{Type: typ, After: enc, SessionID: st.sid, OwnerID: "u-1", CombatantID: ids, At: time.Now()})
		}
	}
	st.hub.Flush()

	frames := map[string][]string{"observer": readAll(t, observer, 400*time.Millisecond), "observer user-level": readAll(t, userLevel, 400*time.Millisecond)}
	for who, fs := range frames {
		if len(fs) == 0 && who == "observer" {
			t.Fatal("the observer received no frames, so nothing was checked")
		}
		for _, f := range fs {
			if l := leaks(f, hiddenSecrets); len(l) > 0 {
				t.Errorf("%s frame leaks %v: %s", who, l, f)
			}
		}
	}
	// The hidden combatant's controller may see its own id and name, never the GM-only fields.
	playerFrames := readAll(t, player, 400*time.Millisecond)
	if len(playerFrames) == 0 {
		t.Fatal("the player received no frames")
	}
	for _, f := range playerFrames {
		secrets := gmOnlySecretsNoTag // a state event or head carries view_tag, which is how players order them
		if strings.Contains(f, `"type":"turn.yours"`) || strings.Contains(f, `"type":"turn.next"`) || strings.Contains(f, `"type":"change_request.`) {
			secrets = gmOnlySecrets // a notice carries neither revision nor view_tag
		}
		if l := leaks(f, secrets); len(l) > 0 {
			t.Errorf("the controller's frame leaks %v: %s", l, f)
		}
	}
}
