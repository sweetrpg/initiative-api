package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
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

	code, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4},"conditions":{"add":[{"name":"prone"}]}}`)

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
		"empty condition":    `{"conditions":{"add":[{"name":" "}]}}`,
		"too many":           `{"conditions":{"add":[` + manyConditions(data.MaxConditions+1) + `]}}`,
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
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4},"conditions":{"add":[{"name":"prone"}]}}`)

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

func conditionNames(t *testing.T, r http.Handler, sid string) []string {
	t.Helper()
	body := decodeBody(t, callAs(r, "user-ok", "GET", encPath(sid, encE1)+"/combatants/pc", ""))
	var out []string
	for _, c := range body["conditions"].([]any) {
		out = append(out, c.(map[string]any)["name"].(string))
	}
	slices.Sort(out)
	return out
}

func TestAConditionAddedByTheGMAfterSubmissionSurvivesApproval(t *testing.T) {
	r, db, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"conditions":{"add":[{"name":"prone"}],"remove":["poisoned"]}}`)
	setPC(t, db, sid, func(e *data.Encounter) {
		e.Combatants[0].Conditions = []data.Condition{{Name: "poisoned"}, {Name: "stunned"}}
	})

	if code, b := approve(r, "user-ok", sid, req["id"].(string)); code != 200 {
		t.Fatalf("approve = %d %v", code, b)
	}

	if got := conditionNames(t, r, sid); !slices.Equal(got, []string{"prone", "stunned"}) {
		t.Fatalf("conditions = %v, want the player's prone added, poisoned removed, and the GM's stunned kept", got)
	}
}

func TestAddingAConditionThatAlreadyExistsUpdatesItInPlace(t *testing.T) {
	r, db, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"conditions":{"add":[{"name":"prone","rounds_remaining":2}]}}`)
	setPC(t, db, sid, func(e *data.Encounter) { e.Combatants[0].Conditions = []data.Condition{{Name: "prone"}} })

	approve(r, "user-ok", sid, req["id"].(string))

	if got := conditionNames(t, r, sid); !slices.Equal(got, []string{"prone"}) {
		t.Fatalf("conditions = %v, want one prone", got)
	}
}

func TestATightenedDefinitionDoesNotBlockAConditionsOnlyRequest(t *testing.T) {
	r, db, sid := changeTable(t)
	zero, one, five := 0, 1, 5
	tight := data.GameSystem{ID: data.DefaultSystemID, Name: "No System", Family: "f", Aliases: []string{"none"}, Fields: []data.TrackerField{
		{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 5, Min: &zero, MaxRef: "max_hp"},
		{Key: "max_hp", Label: "Max HP", Kind: data.FieldCounter, Def: 5, Min: &one, Max: &five}, // the stored max_hp 10 is now over the limit
	}}
	if _, err := data.NewSystemRepo(db).Upsert(context.Background(), tight); err != nil {
		t.Fatal(err)
	}

	code, req := submit(t, r, "user-two", sid, "pc", `{"conditions":{"add":[{"name":"prone"}]}}`)
	done, b := approve(r, "user-ok", sid, idOf(req))

	if code != http.StatusCreated || done != 200 {
		t.Fatalf("conditions-only: submit = %d %v, approve = %d %v; a stored value the definition now rejects must not block it", code, req, done, b)
	}
}

func idOf(m map[string]any) string { s, _ := m["id"].(string); return s }

func TestRequestsInTheOldSpellingOfAWholeConditionsListAreRefused(t *testing.T) {
	r, _, sid := changeTable(t)

	if code, b := submit(t, r, "user-two", sid, "pc", `{"conditions":[{"name":"prone"}]}`); code != 400 && code != 422 {
		t.Fatalf("a bare conditions list = %d %v, want it refused", code, b)
	}
}

func seedClaim(t *testing.T, db *fake.DB, sid, id string, claimedAgo time.Duration) {
	t.Helper()
	claimed := time.Now().Add(-claimedAgo)
	insertRequest(t, db, data.ChangeRequest{ID: id, SessionID: sid, EncounterID: encE1, CombatantID: "pc", RequestedBy: "u-2",
		Health: map[string]int{"hp": 4}, Status: data.ChangeApproving, DecidedBy: "u-1", ClaimedAt: &claimed, BaseRevision: 3})
}

func TestAnOldApprovingClaimIsResumedByAnyGMAndAppliedOnce(t *testing.T) {
	r, db, sid := changeTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")
	seedClaim(t, db, sid, "5f00000000000000000000d1", 2*data.ClaimTimeout)

	code, done := approve(r, "user-four", sid, "5f00000000000000000000d1")
	again, _ := approve(r, "user-ok", sid, "5f00000000000000000000d1")

	if code != 200 || done["status"] != "approved" || done["decided_by"] != "u-4" || done["applied_revision"] != float64(4) {
		t.Fatalf("resume = %d %v, want approved by u-4 at revision 4", code, done)
	}
	if h := pcHealth(t, r, sid); h["hp"] != float64(4) || gmRevision(t, r, sid) != 4 {
		t.Errorf("pc health %v revision %v, want hp 4 and one revision bump", h, gmRevision(t, r, sid))
	}
	if again != http.StatusConflict {
		t.Errorf("approve of an approved request = %d, want 409 (terminal)", again)
	}
}

func TestAYoungApprovingClaimIs409ForApproveAndRejectAndChangesNothing(t *testing.T) {
	r, db, sid := changeTable(t)
	seedClaim(t, db, sid, "5f00000000000000000000d2", 5*time.Second)

	code, _ := approve(r, "user-ok", sid, "5f00000000000000000000d2")
	reject := callAs(r, "user-ok", "POST", crPath(sid, "5f00000000000000000000d2", "reject"), `{"reason":"x"}`)

	if code != http.StatusConflict || reject.Code != http.StatusConflict {
		t.Errorf("approve = %d, reject = %d on a claim in progress; want 409 and 409", code, reject.Code)
	}
	if h := pcHealth(t, r, sid); h["hp"] != float64(9) || gmRevision(t, r, sid) != 3 {
		t.Errorf("pc health %v revision %v: a claim in progress must change nothing", h, gmRevision(t, r, sid))
	}
}

func TestAnApprovingRequestShowsAsPendingAndHidesTheClaim(t *testing.T) {
	r, db, sid := changeTable(t)
	seedClaim(t, db, sid, "5f00000000000000000000d3", 5*time.Second)

	gm := requests(t, r, "user-ok", sid, "?status=pending")
	player := requests(t, r, "user-two", sid, "")

	if len(gm) != 1 || gm[0]["status"] != "pending" || len(player) != 1 || player[0]["status"] != "pending" {
		t.Fatalf("GM pending list %v, player list %v; want the approving request shown as pending to both", gm, player)
	}
	if _, leaks := gm[0]["claimed_at"]; leaks {
		t.Error("the claim time is exposed")
	}
	if _, leaks := gm[0]["decided_by"]; leaks {
		t.Error("the claiming approver is exposed while the request shows as pending")
	}
}

func TestAFailedUnclaimLeavesAClaimThatIsResumedOnceItIsOld(t *testing.T) {
	r, db, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database {
		return failingUpdates{d, data.CollectionChangeRequests, func(u bson.D) error {
			if setsStatus(u, "pending") {
				return errors.New("store down")
			}
			return nil
		}}
	})
	seedSystem(t, db)
	setPC(t, db, sid, func(*data.Encounter) {})
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	locked := time.Now()
	setPC(t, db, sid, func(e *data.Encounter) { e.LockedAt = &locked })
	if code, _ := approve(r, "user-ok", sid, idOf(req)); code != http.StatusForbidden {
		t.Fatalf("approve on a locked encounter = %d, want 403", code)
	}
	setPC(t, db, sid, func(*data.Encounter) {})

	young, _ := approve(r, "user-ok", sid, idOf(req))
	old := time.Now().Add(-2 * data.ClaimTimeout)
	var discard data.ChangeRequest
	if err := db.Fake(data.CollectionChangeRequests).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: idOf(req)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "claimed_at", Value: old}}}}, &discard); err != nil {
		t.Fatal(err)
	}
	code, done := approve(r, "user-ok", sid, idOf(req))

	if young != http.StatusConflict || code != 200 || done["applied_revision"] == nil || pcHealth(t, r, sid)["hp"] != float64(4) {
		t.Fatalf("young claim = %d, aged claim = %d %v, hp %v; want 409 then a completed approval", young, code, done, pcHealth(t, r, sid)["hp"])
	}
}

func TestOnlyTheChangedHealthKeysAreValidated(t *testing.T) {
	r, db, sid := changeTable(t)
	setPC(t, db, sid, func(e *data.Encounter) { e.Combatants[0].Health = map[string]int{"hp": 99, "max_hp": 10} }) // stored hp is over max_hp

	other, _ := submit(t, r, "user-two", sid, "pc", `{"health":{"max_hp":12}}`)
	bad, _ := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":100}}`)

	if other != http.StatusCreated || bad != http.StatusUnprocessableEntity {
		t.Fatalf("a change to another key = %d, a new invalid value = %d; want 201 and 422", other, bad)
	}
}

func insertRequest(t *testing.T, db *fake.DB, req data.ChangeRequest) {
	t.Helper()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}
	if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// failing wraps one collection's FindOneAndUpdate with gate; a non-nil error is returned instead
// of running the update.
type failingUpdates struct {
	store.Database
	collection string
	gate       func(update bson.D) error
}

func (d failingUpdates) Collection(name string) store.Collection {
	c := d.Database.Collection(name)
	if name == d.collection {
		return gatedUpdate{c, d.gate}
	}
	return c
}

type gatedUpdate struct {
	store.Collection
	gate func(bson.D) error
}

func (c gatedUpdate) FindOneAndUpdate(ctx context.Context, f, u bson.D, out any) error {
	if err := c.gate(u); err != nil {
		return err
	}
	return c.Collection.FindOneAndUpdate(ctx, f, u, out)
}

func setsStatus(u bson.D, status string) bool {
	for _, op := range u {
		if set, ok := op.Value.(bson.D); ok && op.Key == "$set" {
			for _, e := range set {
				if e.Key == "status" && e.Value == status {
					return true
				}
			}
		}
	}
	return false
}

func TestApprovalRetriesAnEncounterThatMovedOnThreeTimes(t *testing.T) {
	for _, tt := range []struct {
		misses   int
		wantCode int
		pending  int
	}{{2, 200, 0}, {3, http.StatusConflict, 1}} {
		var misses atomic.Int64
		r, db, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database {
			return failingUpdates{d, data.CollectionEncounters, func(u bson.D) error {
				if misses.Load() < int64(tt.misses) && !setsStatus(u, "x") {
					misses.Add(1)
					return store.ErrNotFound // as if another writer got the revision first
				}
				return nil
			}}
		})
		seedSystem(t, db)
		setPC(t, db, sid, func(*data.Encounter) {})
		misses.Store(0)
		_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

		code, b := approve(r, "user-ok", sid, idOf(req))

		if code != tt.wantCode || len(requests(t, r, "user-ok", sid, "?status=pending")) != tt.pending {
			t.Errorf("%d conflicts: approve = %d %v, pending %d; want %d and %d pending", tt.misses, code, b, len(requests(t, r, "user-ok", sid, "?status=pending")), tt.wantCode, tt.pending)
		}
	}
}

func TestApproveAndRejectRacingLeaveExactlyOneOutcome(t *testing.T) {
	for range 10 {
		r, _, sid := changeTable(t)
		_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() { defer wg.Done(); codes[0], _ = approve(r, "user-ok", sid, idOf(req)) }()
		go func() {
			defer wg.Done()
			codes[1] = callAs(r, "user-ok", "POST", crPath(sid, idOf(req), "reject"), `{"reason":"no"}`).Code
		}()
		wg.Wait()

		hp, rev := pcHealth(t, r, sid)["hp"], gmRevision(t, r, sid)
		switch {
		case codes[0] == 200 && codes[1] == http.StatusConflict:
			if hp != float64(4) || rev != 4 {
				t.Fatalf("approved but hp %v revision %v", hp, rev)
			}
		case codes[1] == 200 && codes[0] == http.StatusConflict:
			if hp != float64(9) || rev != 3 {
				t.Fatalf("rejected but hp %v revision %v: a rejected request changed the combatant", hp, rev)
			}
		default:
			t.Fatalf("approve = %d, reject = %d; want exactly one 200 and one 409", codes[0], codes[1])
		}
	}
}

func TestARequestFromAnotherEncounterOrSessionIsNotFoundUnderAValidURL(t *testing.T) {
	r, db, sid := changeTable(t)
	for id, req := range map[string]data.ChangeRequest{
		"5f00000000000000000000c1": {SessionID: sid, EncounterID: "5f00000000000000000000ff"},
		"5f00000000000000000000c2": {SessionID: foreignSession, EncounterID: encE1},
	} {
		req.ID, req.CombatantID, req.RequestedBy, req.Status, req.CreatedAt = id, "pc", "u-2", data.ChangePending, time.Now()
		req.Health = map[string]int{"hp": 1}
		if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		approveCode, _ := approve(r, "user-ok", sid, id)
		reject := callAs(r, "user-ok", "POST", crPath(sid, id, "reject"), `{"reason":"x"}`)
		if approveCode != 404 || reject.Code != 404 {
			t.Errorf("request %s in %s/%s: approve = %d, reject = %d; want 404", id, req.SessionID, req.EncounterID, approveCode, reject.Code)
		}
	}
}

func TestAPlayerCannotRejectAndAnotherPlayersReasonStaysPrivate(t *testing.T) {
	r, db, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	rejected := data.ChangeRequest{ID: "5f00000000000000000000c3", SessionID: sid, EncounterID: encE1, CombatantID: "orc", RequestedBy: "u-9",
		Status: data.ChangeRejected, Reason: "private reason for u-9", CreatedAt: time.Now()}
	if err := db.Fake(data.CollectionChangeRequests).InsertOne(context.Background(), rejected); err != nil {
		t.Fatal(err)
	}

	w := callAs(r, "user-two", "POST", crPath(sid, idOf(req), "reject"), `{"reason":"I changed my mind"}`)
	list := callAs(r, "user-two", "GET", crPath(sid), "")

	if w.Code != http.StatusForbidden {
		t.Errorf("a player rejecting = %d, want 403", w.Code)
	}
	if strings.Contains(list.Body.String(), "private reason") || strings.Contains(list.Body.String(), "u-9") {
		t.Errorf("a player's list leaks another player's request: %s", list.Body)
	}
}

func TestSubmissionIsRefusedOnALockedOrInactiveEncounterOrSession(t *testing.T) {
	now := time.Now()
	tests := map[string]struct {
		arrange func(t *testing.T, db *fake.DB, sid string)
		code    string
	}{
		"locked encounter": {func(t *testing.T, db *fake.DB, sid string) {
			setPC(t, db, sid, func(e *data.Encounter) { e.LockedAt = &now })
		}, "entitlement_required"},
		"inactive encounter": {func(t *testing.T, db *fake.DB, sid string) {
			setPC(t, db, sid, func(e *data.Encounter) { e.InactiveAt = &now })
		}, "item_inactive"},
		"inactive session": {func(t *testing.T, db *fake.DB, sid string) {
			var discard data.Session
			err := db.Fake(data.CollectionSessions).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: sid}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "inactive_at", Value: now}}}}, &discard)
			if err != nil {
				t.Fatal(err)
			}
		}, "item_inactive"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r, db, sid := changeTable(t)
			tt.arrange(t, db, sid)

			code, b := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

			if code != http.StatusForbidden || b["code"] != tt.code {
				t.Fatalf("= %d %v, want 403 %s", code, b, tt.code)
			}
		})
	}
}

func TestRemovingOrDemotingAPlayerCancelsTheirPendingRequests(t *testing.T) {
	steps := map[string]func(r http.Handler, sid string) *httptest.ResponseRecorder{
		"removed": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return callAs(r, "user-ok", "DELETE", "/sessions/"+sid+"/members/u-2", "")
		},
		"demoted": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"observer"}`)
		},
		"left": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", "")
		},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			r, _, sid := changeTable(t)
			_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

			if w := step(r, sid); w.Code >= 300 {
				t.Fatalf("%s = %d %s", name, w.Code, w.Body)
			}

			if got := requests(t, r, "user-ok", sid, "?status=cancelled"); len(got) != 1 || got[0]["id"] != idOf(req) {
				t.Fatalf("cancelled = %v, want the player's request", got)
			}
			if code, _ := approve(r, "user-ok", sid, idOf(req)); code != http.StatusConflict {
				t.Errorf("approving a cancelled request = %d, want 409", code)
			}
		})
	}
}

func TestChangeRequestResponsesCarryNoEncounterData(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	approved := callAs(r, "user-ok", "POST", crPath(sid, idOf(req), "approve"), "")
	_, second := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":3}}`)
	rejected := callAs(r, "user-ok", "POST", crPath(sid, idOf(second), "reject"), `{"reason":"no"}`)
	list := callAs(r, "user-ok", "GET", crPath(sid), "")

	for name, body := range map[string]string{"submit": mustJSON(req), "approve": approved.Body.String(), "reject": rejected.Body.String(), "list": list.Body.String()} {
		for _, key := range []string{`"combatants"`, `"notes"`, `"turn_queue"`, `"initiative"`, `"controller_user_ids"`} {
			if strings.Contains(body, key) {
				t.Errorf("%s response carries %s: %s", name, key, body)
			}
		}
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestRejectionReasonsAreCleanedAndCapped(t *testing.T) {
	r, _, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

	long := callAs(r, "user-ok", "POST", crPath(sid, idOf(req), "reject"), `{"reason":"`+strings.Repeat("x", 501)+`"}`)
	clean := callAs(r, "user-ok", "POST", crPath(sid, idOf(req), "reject"), `{"reason":"too\u0000 risky\u001b\n"}`)

	if long.Code != http.StatusUnprocessableEntity {
		t.Errorf("a 501 character reason = %d, want 422", long.Code)
	}
	if b := decodeBody(t, clean); clean.Code != 200 || b["reason"] != "too risky" {
		t.Errorf("reason = %v (%d), want control characters stripped", b["reason"], clean.Code)
	}
}

func TestAMalformedEncounterIDNeverReachesTheChangeRequestQueries(t *testing.T) {
	r, _, sid := changeTable(t)
	for _, id := range []string{"not-an-id", `{"$ne":""}`, "5f00000000000000000000e1%00"} {
		list := callAs(r, "user-ok", "GET", "/sessions/"+sid+"/encounters/"+id+"/change-requests", "")
		approve := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/encounters/"+id+"/change-requests/5f00000000000000000000aa/approve", "")
		if list.Code != 404 || approve.Code != 404 {
			t.Errorf("encounter id %q: list = %d, approve = %d; want 404", id, list.Code, approve.Code)
		}
	}
}

// cancelling cancels the request context at a chosen point of an approval, and makes the
// change_requests collection refuse any update made with a cancelled context, as a dropped
// connection would.
type cancelling struct {
	store.Database
	cancel      context.CancelFunc
	beforeWrite bool // cancel when the encounter write is attempted; otherwise right after it succeeds
}

func (d cancelling) Collection(name string) store.Collection {
	c := d.Database.Collection(name)
	switch name {
	case data.CollectionEncounters:
		return cancelOnWrite{c, d}
	case data.CollectionChangeRequests:
		return refuseCancelled{c}
	}
	return c
}

type cancelOnWrite struct {
	store.Collection
	d cancelling
}

func (c cancelOnWrite) FindOneAndUpdate(ctx context.Context, f, u bson.D, out any) error {
	sets := false
	for _, op := range u {
		if set, ok := op.Value.(bson.D); ok && op.Key == "$set" {
			for _, e := range set {
				sets = sets || e.Key == "combatants"
			}
		}
	}
	if sets && c.d.beforeWrite {
		c.d.cancel()
	}
	err := c.Collection.FindOneAndUpdate(ctx, f, u, out)
	if sets && !c.d.beforeWrite && err == nil {
		c.d.cancel()
	}
	return err
}

type refuseCancelled struct{ store.Collection }

func (c refuseCancelled) FindOneAndUpdate(ctx context.Context, f, u bson.D, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Collection.FindOneAndUpdate(ctx, f, u, out)
}

func callWithContext(r http.Handler, ctx context.Context, token, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func cancellingTable(t *testing.T, beforeWrite bool) (http.Handler, *fake.DB, string, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, db, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database { return cancelling{d, cancel, beforeWrite} })
	seedSystem(t, db)
	setPC(t, db, sid, func(*data.Encounter) {})
	return r, db, sid, ctx
}

func TestADisconnectAfterTheEncounterWriteStillRecordsTheApproval(t *testing.T) {
	r, _, sid, ctx := cancellingTable(t, false)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)

	callWithContext(r, ctx, "user-ok", "POST", crPath(sid, idOf(req), "approve"))

	got := requests(t, r, "user-ok", sid, "?status=approved")
	if len(got) != 1 || got[0]["applied_revision"] != float64(4) || pcHealth(t, r, sid)["hp"] != float64(4) {
		t.Fatalf("approved = %v, hp %v; want the applied change recorded as approved even though the caller went away", got, pcHealth(t, r, sid)["hp"])
	}
}

func TestADisconnectDuringAFailedWriteStillReleasesTheClaim(t *testing.T) {
	r, db, sid, ctx := cancellingTable(t, true)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	locked := time.Now()
	setPC(t, db, sid, func(e *data.Encounter) { e.LockedAt = &locked })

	callWithContext(r, ctx, "user-ok", "POST", crPath(sid, idOf(req), "approve"))

	if got := requests(t, r, "user-ok", sid, "?status=pending"); len(got) != 1 {
		t.Fatalf("pending = %v, want the claim released to pending, not stranded approving", got)
	}
	var stuck int
	for _, d := range db.Fake(data.CollectionChangeRequests).Docs() {
		if d["status"] == data.ChangeApproving {
			stuck++
		}
	}
	if stuck != 0 {
		t.Errorf("%d requests still approving", stuck)
	}
}

func TestACancelledRequesterCancelsAnApprovingRequestToo(t *testing.T) {
	r, db, sid := changeTable(t)
	seedClaim(t, db, sid, "5f00000000000000000000d5", 5*time.Second)

	if w := callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""); w.Code >= 300 {
		t.Fatalf("leave = %d %s", w.Code, w.Body)
	}

	if got := requests(t, r, "user-ok", sid, "?status=cancelled"); len(got) != 1 || got[0]["id"] != "5f00000000000000000000d5" {
		t.Fatalf("cancelled = %v, want the approving request cancelled with the requester's departure", got)
	}
	if got := requests(t, r, "user-ok", sid, "?status=pending"); len(got) != 0 {
		t.Errorf("pending = %v, want none", got)
	}
}

func TestAnApprovingRequestShowsNoClaimant(t *testing.T) {
	r, db, sid := changeTable(t)
	_, req := submit(t, r, "user-two", sid, "pc", `{"health":{"hp":4}}`)
	var discard data.ChangeRequest
	err := db.Fake(data.CollectionChangeRequests).FindOneAndUpdate(context.Background(), bson.D{{Key: "_id", Value: idOf(req)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: data.ChangeApproving}, {Key: "decided_by", Value: "u-1"}, {Key: "claimed_at", Value: time.Now()}}}}, &discard)
	if err != nil {
		t.Fatal(err)
	}

	got := requests(t, r, "user-two", sid, "")

	if len(got) != 1 || got[0]["status"] != "pending" || got[0]["decided_by"] != nil {
		t.Fatalf("an approving request as the player sees it = %v, want pending with no decided_by", got)
	}
}
