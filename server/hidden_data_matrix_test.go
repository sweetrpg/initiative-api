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
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	proposedValue   = "7654321" // a health value only a change request proposes; it is nobody's but the GM's and the requester's
	secretCondition = "SecretCurse"
	secretReason    = "GM-only reason"
	lockedRevision  = 987654321    // the revision on a change only GM-level viewers may hear of; it names the change in a GM's frame
	markerName      = "end-marker" // the last notice each stream receives, so a reader knows the run is over
	secretRef       = "gm-ref-secret"
	secretSource    = "src-secret-char"
	removedName     = "RemovedSecret"
)

// the proposed values and decision text of a change request.
var changeSecrets = []string{proposedValue, secretCondition, secretReason}

// GM-only fields of a combatant or session, which no viewer below GM level reads.
var gmFieldSecrets = []string{secretRef, secretSource, removedName, `"external_refs"`, `"source_character_id"`, `"health_visibility"`, `"allow_co_gm_create_encounter"`, `"removed":`}

// valueSecrets are the fixture values that no route may return to a player or observer, wherever the
// route lives; the field names below are meaningful only in session and encounter bodies.
var valueSecrets = append([]string{secretName, secretNote, "GM only orc note", secretRef, secretSource, removedName}, changeSecrets...)

const lockedMark = "987654321"

// everything an observer must never read in a frame or bus event.
var hiddenSecrets = append([]string{secretName, secretNote, `"hid"`, "GM only orc note", `"initiative"`, `"health"`, `"current":31`, `"revision"`, `"owner_user_id"`, "updated_at", lockedMark}, append(append([]string{}, changeSecrets...), gmFieldSecrets...)...)

// restHidden is what a REST body for a member who cannot see the hidden combatant must not hold. A
// visible combatant's own health and a session's own revision are legitimate in a REST body, so the
// hidden one's value (31) stands for its health; encounter bodies also carry no revision, owner or
// timestamps (their consumers order by view_tag).
var restHidden = append([]string{secretName, secretNote, `"hid"`, "GM only orc note", `"initiative"`, `"current":31`}, append(append([]string{}, changeSecrets...), gmFieldSecrets...)...)
var encounterHidden = append(append([]string{}, restHidden...), `"revision"`, `"owner_user_id"`, "updated_at")

// what even the hidden combatant's own controller must never read (its id and name are theirs).
var gmOnlySecrets = append([]string{secretNote, "GM only orc note", `"initiative"`, `"health"`, `"current":31`, `"revision"`, `"view_tag"`, `"owner_user_id"`, "updated_at", lockedMark}, append(append([]string{}, changeSecrets...), gmFieldSecrets...)...)

var gmOnlySecretsNoTag = without(gmOnlySecrets, `"view_tag"`)

func without(all []string, drop string) []string {
	var out []string
	for _, s := range all {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func leaks(body string, secrets []string) []string {
	var out []string
	for _, s := range secrets {
		if strings.Contains(body, s) {
			out = append(out, s)
		}
	}
	return out
}

// secretRequest is a change request whose proposed values and conditions are the GM's to see.
func secretRequest(sid, by string) data.ChangeRequest {
	return data.ChangeRequest{
		ID: "5f00000000000000000000c1", SessionID: sid, EncounterID: encE1, CombatantID: "pc", RequestedBy: by,
		Health: map[string]int{"hp": 7654321}, AddConditions: []data.Condition{{Name: secretCondition}},
		Status: data.ChangePending, BaseRevision: 3, CreatedAt: time.Unix(1_700_000_000, 0),
	}
}

// arrangeMatrix fills the standard fixture with what only the GM may see (an external reference, a
// source character, a removed combatant, the co-GM create flag) and with change requests: one of
// u-9's carrying values nobody else may read, and one of u-2's own. controls says whether u-2
// controls a combatant; when false the player is a plain ViewPlayer.
func arrangeMatrix(t *testing.T, db *fake.DB, sid string, controls bool) {
	t.Helper()
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) {
		for i := range e.Combatants {
			e.Combatants[i].ExternalRefs = map[string]string{"foundry": secretRef}
			e.Combatants[i].SourceCharacterID = secretSource
			if !controls {
				e.Combatants[i].ControllerUserIDs = nil
			}
		}
		e.Combatants = append(e.Combatants, data.Combatant{ID: "gone", Name: removedName, Kind: data.KindAdversary, Removed: true, Notes: secretNote})
	})
	var sess data.Session
	if err := db.Fake(data.CollectionSessions).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: sid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "allow_co_gm_create_encounter", Value: true}}}}, &sess); err != nil {
		t.Fatal(err)
	}
	others := secretRequest(sid, "u-9")
	approved, rejected := others, others
	approved.ID, approved.Status, approved.AppliedRevision = "5f00000000000000000000c2", data.ChangeApproved, 4
	rejected.ID, rejected.Status, rejected.Reason = "5f00000000000000000000c3", data.ChangeRejected, secretReason
	own := secretRequest(sid, "u-2")
	own.ID, own.Health, own.AddConditions = "5f00000000000000000000c4", map[string]int{"hp": 11}, nil
	for _, r := range []data.ChangeRequest{others, approved, rejected, own} {
		if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

var nonEncounterGETs = map[string]bool{
	"/status/ping": true, "/status/health": true, "/themes": true, "/themes/packaged": true, "/themes/:id": true,
	"/encounters": true, "/encounters/:id": true, // the caller's own private encounters, owner-scoped
	"/entitlements": true, "/rosters": true, "/rosters/:id": true, "/game-systems": true, "/game-systems/:id": true,
	"/credentials": true, "/combatants": true, "/admin/credentials": true, "/admin/credentials/deletions": true, "/probe": true,
	"/streams": true,
}

// wantGET is the status a valid read of a session route answers each viewer.
func wantGET(token, routePath, cid string) int {
	switch {
	case strings.Contains(routePath, ":cid") && (cid == "hid" || cid == "gone"):
		return http.StatusNotFound // a combatant they cannot see does not exist for them
	case strings.HasSuffix(routePath, "/change-requests") && token == "user-three":
		return http.StatusForbidden // an observer has no change requests
	case strings.HasSuffix(routePath, "/invites"), strings.HasSuffix(routePath, "/requests"), strings.HasSuffix(routePath, "/blocks"):
		return http.StatusForbidden // the GM's to manage
	}
	return http.StatusOK
}

func TestEveryRESTRouteThatCarriesSessionDataHidesTheHiddenCombatantFromPlayersAndObservers(t *testing.T) {
	for name, controls := range map[string]bool{"player controlling a combatant": true, "player controlling none": false} {
		t.Run(name, func(t *testing.T) {
			st := newStreamTable(t, StreamOptions{}, 0)
			arrangeMatrix(t, st.db, st.sid, controls)
			checked := 0
			for _, rt := range st.r.(*gin.Engine).Routes() {
				if rt.Method != http.MethodGet {
					continue
				}
				session := strings.HasPrefix(rt.Path, "/sessions")
				if !session && !nonEncounterGETs[rt.Path] {
					t.Errorf("GET %s is new: classify it as carrying session data (and so covered here) or as another resource", rt.Path)
				}
				id := encE1
				if session {
					id = st.sid
				}
				for _, cid := range []string{"pc", "hid", "orc", "gone"} {
					path := strings.NewReplacer(":id", id, ":eid", encE1, ":cid", cid).Replace(rt.Path)
					for _, token := range []string{"user-two", "user-three"} {
						w := callAs(st.r, token, "GET", path, "")
						checked++
						secrets := valueSecrets
						if session {
							secrets = restHidden
							if strings.Contains(path, "/encounters") {
								secrets = encounterHidden
							}
						}
						if l := leaks(w.Body.String(), secrets); len(l) > 0 {
							t.Errorf("%s GET %s = %d leaks %v: %s", token, path, w.Code, l, w.Body)
						}
						if session {
							if want := wantGET(token, rt.Path, cid); w.Code != want {
								t.Errorf("%s GET %s = %d, want %d: %s", token, path, w.Code, want, w.Body)
							}
						} else if w.Code >= 500 {
							t.Errorf("%s GET %s = %d", token, path, w.Code)
						}
					}
				}
			}
			if checked == 0 {
				t.Fatal("no REST route was exercised")
			}
		})
	}
}

func TestAPlayerReadsOnlyTheirOwnChangeRequests(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	arrangeMatrix(t, st.db, st.sid, true)
	path := encPath(st.sid, encE1) + "/change-requests"

	own := callAs(st.r, "user-two", "GET", path, "").Body.String()
	if !strings.Contains(own, "5f00000000000000000000c4") || strings.Contains(own, "5f00000000000000000000c1") {
		t.Errorf("a player's change requests = %s, want only their own", own)
	}
	if gm := callAs(st.r, "user-ok", "GET", path, "").Body.String(); !strings.Contains(gm, proposedValue) {
		t.Errorf("the GM's change requests = %s, want the proposed values (the fixture is not vacuous)", gm)
	}
}

func TestWritesAnswerPlayersAndObserversWithoutHiddenData(t *testing.T) {
	cr := encPath("SID", encE1) + "/combatants/%s/change-requests"
	ctl := encPath("SID", encE1) + "/combatants/%s/controllers"
	cases := []struct {
		name, token, method, path, body string
		want                            int
	}{
		{"player proposes a change to their combatant", "user-two", "POST", fmtPath(cr, "pc"), `{"health":{"hp":4}}`, http.StatusCreated},
		{"player proposes a change to a hidden combatant", "user-two", "POST", fmtPath(cr, "hid"), `{"health":{"hp":4}}`, http.StatusNotFound},
		{"observer proposes a change", "user-three", "POST", fmtPath(cr, "pc"), `{"health":{"hp":4}}`, http.StatusForbidden},
		{"player assigns controllers", "user-two", "PUT", fmtPath(ctl, "pc"), `{"controller_user_ids":["u-3"],"revision":3}`, http.StatusForbidden},
		{"player assigns the hidden combatant's controllers", "user-two", "PUT", fmtPath(ctl, "hid"), `{"controller_user_ids":["u-2"],"revision":3}`, http.StatusForbidden},
		{"observer assigns controllers", "user-three", "PUT", fmtPath(ctl, "pc"), `{"controller_user_ids":["u-3"],"revision":3}`, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStreamTable(t, StreamOptions{}, 0)
			arrangeMatrix(t, st.db, st.sid, true)
			w := callAs(st.r, c.token, c.method, strings.ReplaceAll(c.path, "SID", st.sid), c.body)
			if w.Code != c.want {
				t.Fatalf("%s %s = %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
			}
			if l := leaks(w.Body.String(), encounterHidden); len(l) > 0 {
				t.Errorf("the %d answer leaks %v: %s", w.Code, l, w.Body)
			}
		})
	}
}

func fmtPath(format, cid string) string { return strings.Replace(format, "%s", cid, 1) }

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

// hiddenNext is hiddenControlledBy with the hidden combatant on deck instead of up.
func hiddenNext(enc data.Encounter, user string) data.Encounter {
	enc = hiddenControlledBy(enc, user)
	enc.CurrentCombatantID, enc.TurnQueue = "pc", []string{"pc", "hid", "orc"}
	return enc
}

// uncontrolled leaves no combatant to u-2, so the player is a plain ViewPlayer.
func uncontrolled(enc data.Encounter, _ string) data.Encounter {
	cs := append([]data.Combatant(nil), enc.Combatants...)
	for i := range cs {
		cs[i].ControllerUserIDs = nil
	}
	enc.Combatants = cs
	return enc
}

// scenarios are the encounter states emitted to every viewer, each as the document after a change.
var scenarios = map[string]func(data.Encounter, string) data.Encounter{
	"the player controls the hidden combatant, which is up":      hiddenControlledBy,
	"the player controls the hidden combatant, which is on deck": hiddenNext,
	"the player controls nothing":                                uncontrolled,
}

// emitEverything sends every event type, each naming every mix of hidden and visible combatants,
// then the same with the change only GM-level viewers may hear of, and every change request notice.
func emitEverything(ctx context.Context, hub *stream.Hub, sid string, enc data.Encounter) {
	for _, typ := range allEventTypes {
		for _, ids := range [][]string{{"hid"}, {"hid", "orc"}, {"orc"}, nil} {
			origin := data.Origin{Kind: data.OriginUser, ClientID: "ios-7", ActorID: "u-1"}
			hub.Emit(ctx, data.Change{Type: typ, After: enc, SessionID: sid, OwnerID: "u-1", CombatantID: ids, Origin: origin, At: time.Now()})
		}
	}
	locked := enc
	locked.Revision = lockedRevision
	for _, typ := range allEventTypes {
		hub.Emit(ctx, data.Change{Type: typ, After: locked, SessionID: sid, OwnerID: "u-1", GMOnly: true, At: time.Now()})
	}
	req := secretRequest(sid, "u-2")
	hub.ChangeSubmitted(ctx, req)
	approved, rejected := req, req
	now := time.Now()
	approved.Status, approved.DecidedBy, approved.DecidedAt, approved.AppliedRevision = data.ChangeApproved, "u-1", &now, 4
	rejected.Status, rejected.DecidedBy, rejected.DecidedAt, rejected.Reason = data.ChangeRejected, "u-1", &now, secretReason
	hub.ChangeDecided(ctx, approved)
	hub.ChangeDecided(ctx, rejected)
	hub.Notify(ctx, data.Notice{Type: data.NoticeTurnNext, SessionID: sid, EncounterID: encE1, Users: []string{"u-1", "u-2", "u-3"}, At: now,
		Data: map[string]any{"combatant_id": "pc", "combatant_name": markerName, "round": 1}})
}

func countType(frames []string, typ string) int {
	n := 0
	for _, f := range frames {
		if strings.Contains(f, `"type":"`+typ+`"`) {
			n++
		}
	}
	return n
}

func anyContains(frames []string, s string) bool {
	for _, f := range frames {
		if strings.Contains(f, s) {
			return true
		}
	}
	return false
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
	req := secretRequest(n.sid, "u-2")
	n.hub.ChangeSubmitted(context.Background(), req)
	req.Status = data.ChangeApproved
	n.hub.ChangeDecided(context.Background(), req)
	n.hub.Flush()

	if got := notices(drain(gm)); len(got) != 1 || got[0].Type != data.NoticeChangeSubmitted {
		t.Errorf("the GM, who controls nothing, got %+v, want only the submitted change request", got)
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
		if len(got) < 2 {
			t.Fatalf("the controller's %s got %d notices, want the turn and the decision", name, len(got))
		}
		for _, e := range got {
			if l := leaks(js(e), gmOnlySecrets); len(l) > 0 {
				t.Errorf("the controller's %s notice leaks %v: %s", name, l, js(e))
			}
		}
	}
}

// busEvents waits until the marker notice, the last thing emitted, has been published: the bus
// queue is first in, first out, so everything emitted before it is already there.
func busEvents(t *testing.T, bus *captureBus) []stream.Event {
	t.Helper()
	waitFor(t, "the marker notice to reach the bus", func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		for _, ev := range bus.events {
			if strings.Contains(js(ev), markerName) {
				return true
			}
		}
		return false
	})
	bus.mu.Lock()
	defer bus.mu.Unlock()
	return append([]stream.Event(nil), bus.events...)
}

func TestTheBusEnvelopeCarriesNoHiddenDataForStateEventsOrNotices(t *testing.T) {
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			n := newNoticeTable(t)
			bus := &captureBus{}
			n.hub.AttachBus(bus, 1024)
			t.Cleanup(n.hub.Close)
			emitEverything(context.Background(), n.hub, n.sid, scenario(n.encounter(t), "u-2"))

			var sawNotice, sawSubmitted, sawDecided bool
			for _, ev := range busEvents(t, bus) {
				body := js(ev)
				if strings.Contains(body, lockedMark) {
					t.Errorf("a change only GM-level viewers may hear of reached the bus: %s", body)
				}
				if ev.UserID == "" { // a state event: any consumer may read it, so it is the observer's view
					if l := leaks(body, append(hiddenSecrets, "ios-7")); len(l) > 0 {
						t.Errorf("a bus state event %s leaks %v: %s", ev.Type, l, body)
					}
					continue
				}
				sawNotice = true
				want := ev.UserID // the marker is addressed to everyone
				if !strings.Contains(body, markerName) {
					want = "u-2"
				}
				if ev.Type == data.NoticeChangeSubmitted {
					want, sawSubmitted = "u-1", true
				}
				if ev.Type == data.NoticeChangeApproved || ev.Type == data.NoticeChangeRejected {
					sawDecided = true
				}
				if ev.UserID != want {
					t.Errorf("a %s notice was addressed to %s, want %s: %s", ev.Type, ev.UserID, want, body)
				}
				if l := leaks(body, append(gmOnlySecrets, "ios-7")); len(l) > 0 {
					t.Errorf("a bus notice leaks %v: %s", l, body)
				}
			}
			if !sawNotice || !sawSubmitted || !sawDecided {
				t.Errorf("notice, submitted, decided seen = %v %v %v: the notice envelope was not fully exercised", sawNotice, sawSubmitted, sawDecided)
			}
		})
	}
}

// readThrough reads frames until the marker notice arrives, which every stream's viewer is
// addressed by and which is emitted last; an overdue marker fails the test instead of ending a
// quiet period.
func readThrough(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []string
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("reading until the marker: %v (after %d frames)", err, len(out))
		}
		out = append(out, string(b))
		if strings.Contains(string(b), markerName) {
			return out
		}
	}
}

func TestWebSocketFramesToPlayersAndObserversCarryNoHiddenData(t *testing.T) {
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			st := newStreamTable(t, StreamOptions{Heartbeat: time.Hour}, 0)
			arrangeMatrix(t, st.db, st.sid, true)
			gm, _ := st.open(t, "user-ok", "")
			player, _ := st.open(t, "user-two", "")
			observer, _ := st.open(t, "user-three", "")
			playerUserLevel, _ := st.openUserLevel(t, "user-two")
			observerUserLevel, _ := st.openUserLevel(t, "user-three")
			enc := scenario(userStreamEncounter(t, st), "u-2")
			controls := name != "the player controls nothing"

			emitEverything(context.Background(), st.hub, st.sid, enc)
			st.hub.Flush()

			gmFrames := readThrough(t, gm)
			if !anyContains(gmFrames, `"hid"`) || !anyContains(gmFrames, lockedMark) {
				t.Error("the GM did not receive the hidden combatant and the GM-only change, so the emissions were not exercised")
			}
			if countType(gmFrames, data.NoticeChangeSubmitted) != 1 {
				t.Errorf("the GM got %d submitted notices, want 1", countType(gmFrames, data.NoticeChangeSubmitted))
			}
			for who, frames := range map[string][]string{"observer": readThrough(t, observer), "observer user-level": readThrough(t, observerUserLevel)} {
				if len(frames) < 2 && who == "observer" {
					t.Fatalf("the %s received %d frames, so nothing was checked", who, len(frames))
				}
				for _, f := range frames {
					if l := leaks(f, hiddenSecrets); len(l) > 0 && !strings.Contains(f, markerName) {
						t.Errorf("%s frame leaks %v: %s", who, l, f)
					}
				}
				for _, typ := range []string{data.NoticeChangeSubmitted, data.NoticeChangeApproved, data.NoticeChangeRejected, data.NoticeTurnYours} {
					if countType(frames, typ) != 0 {
						t.Errorf("the %s heard a %s notice that is not addressed to them", who, typ)
					}
				}
			}
			for who, c := range map[string]*websocket.Conn{"player": player, "player user-level": playerUserLevel} {
				frames := readThrough(t, c)
				if countType(frames, data.NoticeChangeApproved) != 1 || countType(frames, data.NoticeChangeRejected) != 1 || countType(frames, data.NoticeChangeSubmitted) != 0 {
					t.Errorf("the %s's change notices = approved %d rejected %d submitted %d, want 1, 1, 0", who,
						countType(frames, data.NoticeChangeApproved), countType(frames, data.NoticeChangeRejected), countType(frames, data.NoticeChangeSubmitted))
				}
				if !controls && countType(frames, data.NoticeTurnYours)+countType(frames, data.NoticeTurnNext) != 1 {
					t.Errorf("a player who controls nothing heard turn notices beyond the marker (%s): %v", who, frames)
				}
				checkPlayerFrames(t, who, frames, controls)
			}
		})
	}
}

// checkPlayerFrames: a controller of the hidden combatant may see its id and name, never the
// GM-only fields; a player who controls nothing sees the observer's view, minus the observer-only
// restrictions that apply to the notices addressed to them.
func checkPlayerFrames(t *testing.T, who string, frames []string, controls bool) {
	t.Helper()
	for _, f := range frames {
		if strings.Contains(f, markerName) {
			continue
		}
		secrets := encounterHidden
		if controls {
			secrets = gmOnlySecretsNoTag // a state event or head carries view_tag, which is how players order them
		}
		if strings.Contains(f, `"type":"turn.yours"`) || strings.Contains(f, `"type":"turn.next"`) || strings.Contains(f, `"type":"change_request.`) {
			secrets = gmOnlySecrets // a notice carries neither revision nor view_tag
		}
		if l := leaks(f, secrets); len(l) > 0 {
			t.Errorf("the %s's frame leaks %v: %s", who, l, f)
		}
	}
}
