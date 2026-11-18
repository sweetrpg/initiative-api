package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
)

// writeRouteEvents classifies every write route the router registers: the event type it must emit
// when it succeeds, or "-" for a route that changes no encounter. A new write route that is in
// neither list fails TestEveryEncounterWriteRouteEmitsItsEvent, so it cannot ship without an
// event (initiative-realtime, "Events come from one place").
var writeRouteEvents = map[string]string{
	"POST /encounters":                                                data.EventCreated,
	"PATCH /encounters/:id":                                           data.EventUpdated,
	"DELETE /encounters/:id":                                          data.EventDeleted,
	"POST /encounters/:id/combatants":                                 data.EventCombatantAdded,
	"PATCH /encounters/:id/combatants/:cid":                           data.EventCombatantUpdated,
	"DELETE /encounters/:id/combatants/:cid":                          data.EventCombatantRemoved,
	"POST /encounters/:id/combatants/:cid/restore":                    data.EventCombatantRestored,
	"POST /encounters/:id/start":                                      data.EventStarted,
	"POST /encounters/:id/advance":                                    data.EventTurn,
	"POST /encounters/:id/back":                                       data.EventTurn,
	"POST /encounters/:id/end":                                        data.EventEnded,
	"POST /encounters/:id/reset":                                      data.EventReset,
	"POST /rosters/:id/encounters":                                    data.EventCreated,
	"POST /sessions/:id/encounters":                                   data.EventCreated,
	"PUT /sessions/:id/encounters/:eid/combatants/:cid/controllers":   data.EventControllers,
	"POST /sessions/:id/encounters/:eid/change-requests/:rid/approve": data.EventCombatantUpdated,
	"DELETE /sessions/:id":                                            data.EventReleased,
	"DELETE /sessions/:id/members/:user":                              data.EventControllers,
	"POST /sessions/:id/leave":                                        data.EventControllers,
	"POST /sessions/:id/blocks":                                       data.EventControllers,
	"PATCH /sessions/:id/members/:user":                               data.EventControllers,

	// Routes that change no encounter.
	"POST /encounters/:id/save-as-roster": "-", "POST /sessions/:id/encounters/:eid/change-requests/:rid/reject": "-",
	"POST /sessions/:id/encounters/:eid/combatants/:cid/change-requests": "-",
	"DELETE /admin/credentials/:id":                                      "-", "POST /admin/credentials/reopen": "-", "POST /credentials": "-", "POST /credentials/:id/revoke": "-",
	"DELETE /rosters/:id": "-", "DELETE /rosters/:id/characters/:cid": "-", "PATCH /rosters/:id": "-", "POST /rosters": "-",
	"POST /rosters/:id/characters": "-", "PUT /rosters/:id/characters/:cid": "-",
	"DELETE /sessions/:id/blocks/:user": "-", "DELETE /sessions/:id/invites/:invite": "-", "PATCH /sessions/:id": "-", "POST /sessions": "-",
	"POST /sessions/:id/invites": "-", "POST /sessions/:id/requests/:request/approve": "-", "POST /sessions/:id/requests/:request/reject": "-",
	"POST /sessions/join": "-", "DELETE /themes/:id": "-", "PATCH /themes/:id": "-", "POST /themes": "-",
	// Limit choices only move items the owner picks; the flag writes they cause are covered by
	// TestLockAndInactiveFlagsAreEmittedGMOnly.
	"PUT /entitlements/active/:kind": "-",
}

func TestEveryEncounterWriteRouteEmitsItsEvent(t *testing.T) {
	e := newEventTableLimit(t, "50", nil)
	seen := map[string]bool{}
	for _, route := range e.r.(*gin.Engine).Routes() {
		if route.Path == "/probe" || route.Method == http.MethodGet || route.Method == http.MethodHead || route.Method == http.MethodOptions {
			continue
		}
		key := route.Method + " " + route.Path
		seen[key] = true
		if _, ok := writeRouteEvents[key]; !ok {
			t.Errorf("write route %s is not classified in writeRouteEvents: say which event it emits, or that it changes no encounter", key)
		}
	}
	for key := range writeRouteEvents {
		if !seen[key] {
			t.Errorf("writeRouteEvents lists %s, which is not a registered route", key)
		}
	}

	for key, want := range writeRouteEvents {
		t.Run(key, func(t *testing.T) {
			scenario, ok := routeScenarios[key]
			if !ok {
				t.Fatalf("no scenario drives %s to emit %s", key, want)
			}
			tbl := newEventTableLimit(t, "50", nil)
			req := scenario(t, tbl)
			before := tbl.sink.count()

			w := callAs(tbl.r, req.token, req.method, req.path, req.body)

			if w.Code >= 300 {
				t.Fatalf("%s = %d %s", key, w.Code, w.Body)
			}
			tbl.sink.mu.Lock()
			got := append([]data.Change(nil), tbl.sink.changes[before:]...)
			tbl.sink.mu.Unlock()
			if want == "-" {
				if len(got) != 0 {
					t.Fatalf("%s is classified as changing no encounter but emitted %v", key, changeTypes(got))
				}
				return
			}
			// Exactly one event per committed change: one for each encounter the request changed
			// (the fixture has one encounter in the session).
			if len(got) != 1 || got[0].Type != want {
				t.Fatalf("%s emitted %v, want exactly one %s", key, changeTypes(got), want)
			}
		})
	}
}

func changeTypes(cs []data.Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Type)
	}
	return out
}

type routeCall struct{ token, method, path, body string }

// privateEncounter makes the GM's private encounter with two combatants and returns its id.
func privateEncounter(t *testing.T, tbl eventTable) (id string, rev func() string, cid string) {
	t.Helper()
	enc := newEncounter(t, tbl.r, "user-ok", `{"name":"P","game_system_id":"`+data.DefaultSystemID+`","combatants":[{"name":"A","kind":"pc","initiative":3},{"name":"B","kind":"adversary","initiative":1}]}`)
	id = enc["id"].(string)
	rev = func() string {
		return strconv.Itoa(int(decodeBody(t, callAs(tbl.r, "user-ok", "GET", "/encounters/"+id, ""))["revision"].(float64)))
	}
	return id, rev, enc["combatants"].([]any)[0].(map[string]any)["id"].(string)
}

func started(t *testing.T, tbl eventTable, id string, rev func() string) {
	t.Helper()
	if w := callAs(tbl.r, "user-ok", "POST", "/encounters/"+id+"/start?revision="+rev(), ""); w.Code != 200 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
}

var routeScenarios = map[string]func(*testing.T, eventTable) routeCall{
	// Routes that change no encounter must emit nothing.
	"POST /encounters/:id/save-as-roster": func(t *testing.T, tbl eventTable) routeCall {
		id, _, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/save-as-roster", `{"name":"S"}`}
	},
	"POST /sessions/:id/encounters/:eid/change-requests/:rid/reject": func(t *testing.T, tbl eventTable) routeCall {
		setPC(t, tbl.db, tbl.sid, func(*data.Encounter) {})
		_, req := submit(t, tbl.r, "user-two", tbl.sid, "pc", `{"health":{"hp":4}}`)
		return routeCall{"user-ok", "POST", crPath(tbl.sid, idOf(req), "reject"), `{"reason":"no"}`}
	},
	"POST /sessions/:id/encounters/:eid/combatants/:cid/change-requests": func(t *testing.T, tbl eventTable) routeCall {
		setPC(t, tbl.db, tbl.sid, func(*data.Encounter) {})
		return routeCall{"user-two", "POST", encPath(tbl.sid, encE1) + "/combatants/pc/change-requests", `{"health":{"hp":4}}`}
	},
	"DELETE /admin/credentials/:id": func(t *testing.T, tbl eventTable) routeCall {
		id, _ := issue(t, tbl.r, "user-ok", `["read"]`)
		return routeCall{"admin-ok", "DELETE", "/admin/credentials/" + id, `{"reason":"leaked"}`}
	},
	"POST /admin/credentials/reopen": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"admin-ok", "POST", "/admin/credentials/reopen", ""}
	},
	"POST /credentials": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/credentials", `{"label":"foundry","scopes":["read"]}`}
	},
	"POST /credentials/:id/revoke": func(t *testing.T, tbl eventTable) routeCall {
		id, _ := issue(t, tbl.r, "user-ok", `["read"]`)
		return routeCall{"user-ok", "POST", "/credentials/" + id + "/revoke", ""}
	},
	"POST /rosters": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/rosters", `{"name":"R"}`}
	},
	"PATCH /rosters/:id": func(t *testing.T, tbl eventTable) routeCall {
		id := newRoster(t, tbl.r, "user-ok", `{"name":"R"}`)["id"].(string)
		return routeCall{"user-ok", "PATCH", "/rosters/" + id, `{"revision":1,"name":"R2"}`}
	},
	"DELETE /rosters/:id": func(t *testing.T, tbl eventTable) routeCall {
		id := newRoster(t, tbl.r, "user-ok", `{"name":"R"}`)["id"].(string)
		return routeCall{"user-ok", "DELETE", "/rosters/" + id + "?revision=1", ""}
	},
	"POST /rosters/:id/characters": func(t *testing.T, tbl eventTable) routeCall {
		id := newRoster(t, tbl.r, "user-ok", `{"name":"R"}`)["id"].(string)
		return routeCall{"user-ok", "POST", "/rosters/" + id + "/characters", `{"revision":1,"name":"Orc","kind":"adversary"}`}
	},
	"PUT /rosters/:id/characters/:cid": func(t *testing.T, tbl eventTable) routeCall {
		r := newRoster(t, tbl.r, "user-ok", `{"name":"R","characters":[{"name":"C","kind":"pc"}]}`)
		cid := r["characters"].([]any)[0].(map[string]any)["id"].(string)
		return routeCall{"user-ok", "PUT", "/rosters/" + r["id"].(string) + "/characters/" + cid, `{"revision":1,"name":"C2","kind":"pc"}`}
	},
	"DELETE /rosters/:id/characters/:cid": func(t *testing.T, tbl eventTable) routeCall {
		r := newRoster(t, tbl.r, "user-ok", `{"name":"R","characters":[{"name":"C","kind":"pc"}]}`)
		cid := r["characters"].([]any)[0].(map[string]any)["id"].(string)
		return routeCall{"user-ok", "DELETE", "/rosters/" + r["id"].(string) + "/characters/" + cid + "?revision=1", ""}
	},
	"POST /sessions": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-three", "POST", "/sessions", `{"name":"Other"}`}
	},
	"PATCH /sessions/:id": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "PATCH", "/sessions/" + tbl.sid, `{"name":"Renamed","revision":1}`}
	},
	"POST /sessions/:id/invites": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/sessions/" + tbl.sid + "/invites", ""}
	},
	"DELETE /sessions/:id/invites/:invite": func(t *testing.T, tbl eventTable) routeCall {
		inviteCode(t, tbl.r, "user-ok", tbl.sid)
		items := decodeBody(t, callAs(tbl.r, "user-ok", "GET", "/sessions/"+tbl.sid+"/invites", ""))["items"].([]any)
		return routeCall{"user-ok", "DELETE", "/sessions/" + tbl.sid + "/invites/" + items[0].(map[string]any)["id"].(string), ""}
	},
	"DELETE /sessions/:id/blocks/:user": func(t *testing.T, tbl eventTable) routeCall {
		callAs(tbl.r, "user-ok", "POST", "/sessions/"+tbl.sid+"/blocks", `{"user_id":"u-9"}`)
		return routeCall{"user-ok", "DELETE", "/sessions/" + tbl.sid + "/blocks/u-9", ""}
	},
	"POST /sessions/join": func(t *testing.T, tbl eventTable) routeCall {
		code := inviteCode(t, tbl.r, "user-ok", tbl.sid)
		return routeCall{"user-four", "POST", "/sessions/join", `{"code":"` + code + `"}`}
	},
	"POST /sessions/:id/requests/:request/approve": func(t *testing.T, tbl eventTable) routeCall {
		code := inviteCode(t, tbl.r, "user-ok", tbl.sid)
		req := decodeBody(t, callAs(tbl.r, "user-four", "POST", "/sessions/join", `{"code":"`+code+`"}`))
		return routeCall{"user-ok", "POST", "/sessions/" + tbl.sid + "/requests/" + req["id"].(string) + "/approve", `{"role":"player"}`}
	},
	"POST /sessions/:id/requests/:request/reject": func(t *testing.T, tbl eventTable) routeCall {
		code := inviteCode(t, tbl.r, "user-ok", tbl.sid)
		req := decodeBody(t, callAs(tbl.r, "user-four", "POST", "/sessions/join", `{"code":"`+code+`"}`))
		return routeCall{"user-ok", "POST", "/sessions/" + tbl.sid + "/requests/" + req["id"].(string) + "/reject", `{"reason":"no"}`}
	},
	"POST /themes": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/themes", themeJSON("A")}
	},
	"PATCH /themes/:id": func(t *testing.T, tbl eventTable) routeCall {
		id := decodeBody(t, callAs(tbl.r, "user-ok", "POST", "/themes", themeJSON("A")))["id"].(string)
		return routeCall{"user-ok", "PATCH", "/themes/" + id, `{"revision":1,"name":"B"}`}
	},
	"DELETE /themes/:id": func(t *testing.T, tbl eventTable) routeCall {
		id := decodeBody(t, callAs(tbl.r, "user-ok", "POST", "/themes", themeJSON("A")))["id"].(string)
		return routeCall{"user-ok", "DELETE", "/themes/" + id + "?revision=1", ""}
	},
	"PUT /entitlements/active/:kind": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "PUT", "/entitlements/active/encounters", `{"ids":[]}`}
	},

	"POST /encounters": func(t *testing.T, _ eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/encounters", `{"name":"N","game_system_id":"` + data.DefaultSystemID + `"}`}
	},
	"PATCH /encounters/:id": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "PATCH", "/encounters/" + id + "?revision=" + rev(), `{"name":"P2"}`}
	},
	"DELETE /encounters/:id": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "DELETE", "/encounters/" + id + "?revision=" + rev(), ""}
	},
	"POST /encounters/:id/combatants": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/combatants?revision=" + rev(), `{"name":"C","kind":"pc"}`}
	},
	"PATCH /encounters/:id/combatants/:cid": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, cid := privateEncounter(t, tbl)
		return routeCall{"user-ok", "PATCH", "/encounters/" + id + "/combatants/" + cid + "?revision=" + rev(), `{"notes":"n"}`}
	},
	"DELETE /encounters/:id/combatants/:cid": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, cid := privateEncounter(t, tbl)
		return routeCall{"user-ok", "DELETE", "/encounters/" + id + "/combatants/" + cid + "?revision=" + rev(), ""}
	},
	"POST /encounters/:id/combatants/:cid/restore": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, cid := privateEncounter(t, tbl)
		callAs(tbl.r, "user-ok", "DELETE", "/encounters/"+id+"/combatants/"+cid+"?revision="+rev(), "")
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/combatants/" + cid + "/restore?revision=" + rev(), ""}
	},
	"POST /encounters/:id/start": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/start?revision=" + rev(), ""}
	},
	"POST /encounters/:id/advance": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		started(t, tbl, id, rev)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/advance?revision=" + rev(), ""}
	},
	"POST /encounters/:id/back": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		started(t, tbl, id, rev)
		callAs(tbl.r, "user-ok", "POST", "/encounters/"+id+"/advance?revision="+rev(), "")
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/back?revision=" + rev(), ""}
	},
	"POST /encounters/:id/end": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		started(t, tbl, id, rev)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/end?revision=" + rev(), ""}
	},
	"POST /encounters/:id/reset": func(t *testing.T, tbl eventTable) routeCall {
		id, rev, _ := privateEncounter(t, tbl)
		return routeCall{"user-ok", "POST", "/encounters/" + id + "/reset?revision=" + rev(), ""}
	},
	"POST /rosters/:id/encounters": func(t *testing.T, tbl eventTable) routeCall {
		roster := newRoster(t, tbl.r, "user-ok", `{"name":"R","characters":[{"name":"C","kind":"pc"}]}`)["id"].(string)
		return routeCall{"user-ok", "POST", "/rosters/" + roster + "/encounters", `{"name":"F","game_system_id":"` + data.DefaultSystemID + `"}`}
	},
	"POST /sessions/:id/encounters": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/sessions/" + tbl.sid + "/encounters", newSessionEncounter}
	},
	"PUT /sessions/:id/encounters/:eid/combatants/:cid/controllers": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "PUT", controllersPath(tbl.sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`}
	},
	"POST /sessions/:id/encounters/:eid/change-requests/:rid/approve": func(t *testing.T, tbl eventTable) routeCall {
		setPC(t, tbl.db, tbl.sid, func(*data.Encounter) {})
		_, req := submit(t, tbl.r, "user-two", tbl.sid, "pc", `{"health":{"hp":4}}`)
		return routeCall{"user-ok", "POST", crPath(tbl.sid, idOf(req), "approve"), ""}
	},
	"DELETE /sessions/:id": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "DELETE", "/sessions/" + tbl.sid + "?revision=1", ""}
	},
	"DELETE /sessions/:id/members/:user": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "DELETE", "/sessions/" + tbl.sid + "/members/u-2", ""}
	},
	"POST /sessions/:id/leave": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-two", "POST", "/sessions/" + tbl.sid + "/leave", ""}
	},
	"POST /sessions/:id/blocks": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "POST", "/sessions/" + tbl.sid + "/blocks", `{"user_id":"u-2"}`}
	},
	"PATCH /sessions/:id/members/:user": func(t *testing.T, tbl eventTable) routeCall {
		return routeCall{"user-ok", "PATCH", "/sessions/" + tbl.sid + "/members/u-2", `{"role":"observer"}`}
	},
}
