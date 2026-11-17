package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// changeTable is viewTable with the pc (controlled by u-2) holding health the default system
// defines: hp 9 of max_hp 10.
func changeTable(t *testing.T) (http.Handler, *fake.DB, string) {
	t.Helper()
	r, db, sid := viewTable(t)
	seedSystem(t, db)
	setPC(t, db, sid, func(e *data.Encounter) {})
	return r, db, sid
}

func setPC(t *testing.T, db *fake.DB, sid string, edit func(*data.Encounter)) {
	t.Helper()
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) {
		e.GameSystemID = data.DefaultSystemID
		e.Combatants[0].Health = map[string]int{"hp": 9, "max_hp": 10}
		edit(e)
	})
}

func crPath(sid string, parts ...string) string {
	p := encPath(sid, encE1) + "/change-requests"
	for _, s := range parts {
		p += "/" + s
	}
	return p
}

func submit(t *testing.T, r http.Handler, token, sid, cid, body string) (int, map[string]any) {
	t.Helper()
	w := callAs(r, token, "POST", encPath(sid, encE1)+"/combatants/"+cid+"/change-requests", body)
	return w.Code, decodeBody(t, w)
}

func pcHealth(t *testing.T, r http.Handler, sid string) map[string]any {
	t.Helper()
	body := decodeBody(t, callAs(r, "user-ok", "GET", encPath(sid, encE1)+"/combatants/pc", ""))
	h, _ := body["health"].(map[string]any)
	return h
}

func requests(t *testing.T, r http.Handler, token, sid, query string) []map[string]any {
	t.Helper()
	w := callAs(r, token, "GET", crPath(sid)+query, "")
	if w.Code != 200 {
		t.Fatalf("list as %s = %d %s", token, w.Code, w.Body)
	}
	var out []map[string]any
	for _, it := range decodeBody(t, w)["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestAControllingPlayerSubmitsAChangeAndTheCombatantIsUnchanged(t *testing.T) {
	r, _, sid := changeTable(t)

	code, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4},"conditions":[{"name":"prone"}]}`)

	if code != http.StatusCreated || req["status"] != "pending" || req["requested_by"] != "u-2" || req["base_revision"] != float64(3) || req["combatant_id"] != "pc" {
		t.Fatalf("submit = %d %v, want a pending request by u-2 at revision 3", code, req)
	}
	if h := pcHealth(t, r, sid); h["hp"] != float64(9) || gmRevision(t, r, sid) != 3 {
		t.Errorf("pc health %v revision %v: a submitted request must change nothing", h, gmRevision(t, r, sid))
	}
}

func TestOnlyAControllingPlayerMaySubmit(t *testing.T) {
	r, _, sid := changeTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")
	body := `{"health":{"hp":4}}`
	tests := []struct {
		name, token, cid string
		want             int
	}{
		{"observer", "user-three", "pc", 403},
		{"GM", "user-ok", "pc", 403},
		{"co-GM", "user-four", "pc", 403},
		{"player who does not control it", "user-two", "orc", 403},
		{"hidden combatant the player does not control", "user-two", "hid", 404},
		{"unknown combatant", "user-two", "nope", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, b := submit(t, r, tt.token, sid, tt.cid, body); code != tt.want {
				t.Fatalf("= %d %v, want %d", code, b, tt.want)
			}
		})
	}
}

func TestAStaleOrDemotedControllerCannotSubmit(t *testing.T) {
	t.Run("no longer listed", func(t *testing.T) {
		r, _, sid := changeTable(t)
		if w := callAs(r, "user-ok", "PUT", controllersPath(sid, "pc"), `{"controller_user_ids":[],"revision":3}`); w.Code != 200 {
			t.Fatalf("clear = %d %s", w.Code, w.Body)
		}
		if code, b := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`); code != 403 {
			t.Fatalf("submit after being unassigned = %d %v, want 403", code, b)
		}
	})
	t.Run("demoted to observer with the id still listed", func(t *testing.T) {
		r, db, sid := changeTable(t)
		if w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"observer"}`); w.Code != 200 {
			t.Fatalf("demote = %d %s", w.Code, w.Body)
		}
		setPC(t, db, sid, func(*data.Encounter) {}) // the stale id is back, as a failed scrub would leave it
		if code, b := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`); code != 403 {
			t.Fatalf("submit as a demoted controller = %d %v, want 403", code, b)
		}
	})
}

func TestSubmissionIsValidatedAgainstTheGameSystem(t *testing.T) {
	r, _, sid := changeTable(t)
	tests := map[string]string{
		"above the maximum":  `{"health":{"hp":11}}`,
		"below the minimum":  `{"health":{"hp":-1}}`,
		"undefined field":    `{"health":{"mana":3}}`,
		"nothing proposed":   `{}`,
		"empty condition":    `{"conditions":[{"name":" "}]}`,
		"too many":           `{"conditions":[` + manyConditions(data.MaxConditions+1) + `]}`,
		"a field of its own": `{"health":{"hp":4},"notes":"x"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if code, b := submit(t, r, "user-two", sid, "pc", body); code != 422 {
				t.Fatalf("= %d %v, want 422", code, b)
			}
		})
	}
}

func manyConditions(n int) string {
	s := ""
	for i := range n {
		if i > 0 {
			s += ","
		}
		s += `{"name":"c` + string(rune('a'+i%26)) + `"}`
	}
	return s
}

func TestListShowsAGMEverythingAPlayerOnlyTheirOwnAndAnObserverNothing(t *testing.T) {
	r, db, sid := changeTable(t)
	submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	other := data.ChangeRequest{ID: "5f00000000000000000000c9", SessionID: sid, EncounterID: encE1, CombatantID: "orc", RequestedBy: "u-9",
		Health: map[string]int{"hp": 1}, Status: data.ChangePending, CreatedAt: time.Now()}
	if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	gm := requests(t, r, "user-ok", sid, "")
	own := requests(t, r, "user-two", sid, "")
	obs := callAs(r, "user-three", "GET", crPath(sid), "")

	if len(gm) != 2 || len(own) != 1 || own[0]["requested_by"] != "u-2" {
		t.Errorf("GM sees %d, player sees %v; want 2 and only their own", len(gm), own)
	}
	if obs.Code != http.StatusForbidden {
		t.Errorf("observer list = %d, want 403", obs.Code)
	}
	if got := requests(t, r, "user-ok", sid, "?status=approved"); len(got) != 0 {
		t.Errorf("status=approved listed %v, want none", got)
	}
	if bad := callAs(r, "user-ok", "GET", crPath(sid)+"?status=weird", ""); bad.Code != 400 {
		t.Errorf("bad status = %d, want 400", bad.Code)
	}
}

func approve(r http.Handler, token, sid, id string) (int, map[string]any) {
	w := callAs(r, token, "POST", crPath(sid, id, "approve"), "")
	var b map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return w.Code, b
}

func TestApprovalAppliesTheChangeInTheSameBumpAndRecordsTheApprover(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4},"conditions":[{"name":"prone"}]}`)

	code, done := approve(r, "user-ok", sid, req["id"].(string))

	if code != 200 || done["status"] != "approved" || done["decided_by"] != "u-1" || done["applied_revision"] != float64(4) {
		t.Fatalf("approve = %d %v, want approved by u-1 at revision 4", code, done)
	}
	if h := pcHealth(t, r, sid); h["hp"] != float64(4) || h["max_hp"] != float64(10) || gmRevision(t, r, sid) != 4 {
		t.Errorf("pc health %v revision %v, want hp 4, max_hp kept, revision 4", h, gmRevision(t, r, sid))
	}
	if got := requests(t, r, "user-two", sid, "?status=approved"); len(got) != 1 {
		t.Errorf("the player's approved list = %v, want their request", got)
	}
}

func TestACoGMCanApprove(t *testing.T) {
	r, _, sid := changeTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":2}}`)

	if code, done := approve(r, "user-four", sid, req["id"].(string)); code != 200 || done["decided_by"] != "u-4" {
		t.Fatalf("co-GM approve = %d %v", code, done)
	}
}

func TestApprovalAgainstANewerEncounterRevisionSucceedsWhenTheChangeStillValidates(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	if w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`); w.Code != 200 {
		t.Fatalf("an unrelated edit = %d %s", w.Code, w.Body)
	}

	code, done := approve(r, "user-ok", sid, req["id"].(string))

	if code != 200 || done["applied_revision"] != float64(5) || gmRevision(t, r, sid) != 5 {
		t.Fatalf("approve after the encounter moved on = %d %v, revision %v; want 200 at revision 5", code, done, gmRevision(t, r, sid))
	}
}

func TestAStaleRequestFailsWithATypedErrorAndStaysPending(t *testing.T) {
	staleness := map[string]func(t *testing.T, r http.Handler, db *fake.DB, sid string){
		"the requester no longer controls the combatant": func(t *testing.T, r http.Handler, _ *fake.DB, sid string) {
			if w := callAs(r, "user-ok", "PUT", controllersPath(sid, "pc"), `{"controller_user_ids":[],"revision":3}`); w.Code != 200 {
				t.Fatalf("clear = %d", w.Code)
			}
		},
		"the value no longer validates": func(t *testing.T, _ http.Handler, db *fake.DB, sid string) {
			setPC(t, db, sid, func(e *data.Encounter) { e.Combatants[0].Health = map[string]int{"hp": 3, "max_hp": 5} })
		},
		"the combatant was removed": func(t *testing.T, _ http.Handler, db *fake.DB, sid string) {
			setPC(t, db, sid, func(e *data.Encounter) { e.Combatants[0].Removed = true })
		},
		"the requester was demoted": func(t *testing.T, r http.Handler, db *fake.DB, sid string) {
			if w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"observer"}`); w.Code != 200 {
				t.Fatalf("demote = %d", w.Code)
			}
			setPC(t, db, sid, func(*data.Encounter) {})
		},
	}
	for name, make := range staleness {
		t.Run(name, func(t *testing.T) {
			r, db, sid := changeTable(t)
			_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":8}}`)
			make(t, r, db, sid)
			before := gmRevision(t, r, sid)

			code, b := approve(r, "user-ok", sid, req["id"].(string))

			if code != http.StatusConflict || b["code"] != "change_request_stale" {
				t.Fatalf("approve = %d %v, want 409 change_request_stale", code, b)
			}
			if got := requests(t, r, "user-ok", sid, "?status=pending"); len(got) != 1 {
				t.Errorf("pending = %v, want the request to stay pending", got)
			}
			if gmRevision(t, r, sid) != before {
				t.Error("a stale approval changed the encounter")
			}
		})
	}
}

func TestAFailedEncounterWriteReopensTheRequest(t *testing.T) {
	r, db, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	locked := time.Now()
	setPC(t, db, sid, func(e *data.Encounter) { e.LockedAt = &locked })

	code, b := approve(r, "user-ok", sid, req["id"].(string))

	if code != http.StatusForbidden {
		t.Fatalf("approve on a locked encounter = %d %v, want 403", code, b)
	}
	if got := requests(t, r, "user-ok", sid, "?status=pending"); len(got) != 1 {
		t.Errorf("pending = %v, want the claimed request put back to pending", got)
	}
}

func TestRejectRecordsTheReasonLeavesTheCombatantAndTheRequesterSeesIt(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	id := req["id"].(string)

	noReason := callAs(r, "user-ok", "POST", crPath(sid, id, "reject"), `{"reason":"  "}`)
	w := callAs(r, "user-ok", "POST", crPath(sid, id, "reject"), `{"reason":"You missed the save"}`)

	if noReason.Code != 422 {
		t.Errorf("reject with no reason = %d, want 422", noReason.Code)
	}
	if b := decodeBody(t, w); w.Code != 200 || b["status"] != "rejected" || b["reason"] != "You missed the save" || b["decided_by"] != "u-1" {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	if h := pcHealth(t, r, sid); h["hp"] != float64(9) || gmRevision(t, r, sid) != 3 {
		t.Errorf("pc health %v revision %v: a rejection must leave the combatant unchanged", h, gmRevision(t, r, sid))
	}
	if own := requests(t, r, "user-two", sid, "?status=rejected"); len(own) != 1 || own[0]["reason"] != "You missed the save" {
		t.Errorf("the player's rejected list = %v, want the reason", own)
	}
}

func TestADecidedRequestCannotBeDecidedAgainAndPlayersCannotDecide(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	id := req["id"].(string)

	if w := callAs(r, "user-two", "POST", crPath(sid, id, "approve"), ""); w.Code != http.StatusForbidden {
		t.Errorf("player approves own request = %d, want 403", w.Code)
	}
	if w := callAs(r, "user-three", "POST", crPath(sid, id, "reject"), `{"reason":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("observer rejects = %d, want 403", w.Code)
	}
	if code, _ := approve(r, "user-ok", sid, id); code != 200 {
		t.Fatalf("approve = %d", code)
	}
	again, _ := approve(r, "user-ok", sid, id)
	reject := callAs(r, "user-ok", "POST", crPath(sid, id, "reject"), `{"reason":"too late"}`)

	if again != http.StatusConflict || reject.Code != http.StatusConflict {
		t.Errorf("second approve = %d, reject after approve = %d; want 409 and 409", again, reject.Code)
	}
}

func TestAnUnknownRequestOrEncounterIs404(t *testing.T) {
	r, _, sid := changeTable(t)

	missing, _ := approve(r, "user-ok", sid, "5f00000000000000000000aa")
	other := callAs(r, "user-ok", "POST", encPath(sid, "5f00000000000000000000ff")+"/change-requests/5f00000000000000000000aa/approve", "")
	list := callAs(r, "user-ok", "GET", encPath(sid, "5f00000000000000000000ff")+"/change-requests", "")

	if missing != 404 || other.Code != 404 || list.Code != 404 {
		t.Errorf("unknown request = %d, unknown encounter = %d / %d; want 404s", missing, other.Code, list.Code)
	}
}

func TestPendingRequestsPerPlayerAreCapped(t *testing.T) {
	r, db, sid := changeTable(t)
	for i := range data.MaxPendingChangeRequests {
		req := data.ChangeRequest{ID: "5f0000000000000000" + hex6(i), SessionID: sid, EncounterID: encE1, CombatantID: "pc", RequestedBy: "u-2",
			Health: map[string]int{"hp": 1}, Status: data.ChangePending, CreatedAt: time.Now()}
		if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	if code, b := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`); code != 422 {
		t.Fatalf("submit past the cap = %d %v, want 422", code, b)
	}
}

func hex6(i int) string { return fmt.Sprintf("%06x", i+1) }
