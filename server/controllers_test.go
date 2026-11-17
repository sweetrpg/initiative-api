package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

const encE1 = "5f00000000000000000000e1"

func controllersPath(sid, cid string) string {
	return encPath(sid, encE1) + "/combatants/" + cid + "/controllers"
}

// controllersOf reads a combatant's controller_user_ids as the GM sees them.
func controllersOf(t *testing.T, r http.Handler, sid, cid string) []string {
	t.Helper()
	body := decodeBody(t, callAs(r, "user-ok", "GET", encPath(sid, encE1)+"/combatants/"+cid, ""))
	var out []string
	if ids, ok := body["controller_user_ids"].([]any); ok {
		for _, id := range ids {
			out = append(out, id.(string))
		}
	}
	return out
}

func gmRevision(t *testing.T, r http.Handler, sid string) float64 {
	t.Helper()
	return decodeBody(t, callAs(r, "user-ok", "GET", encPath(sid, encE1), ""))["revision"].(float64)
}

func mineFor(t *testing.T, r http.Handler, token, sid, cid string) bool {
	t.Helper()
	w := callAs(r, token, "GET", encPath(sid, encE1)+"/combatants/"+cid, "")
	if w.Code != 200 {
		t.Fatalf("%s GET combatant %s = %d %s", token, cid, w.Code, w.Body)
	}
	mine, _ := decodeBody(t, w)["mine"].(bool)
	return mine
}

func TestGMAssignsAPlayerToSeveralCombatantsAndACombatantToSeveralControllers(t *testing.T) {
	r, _, sid := viewTable(t)

	one := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)
	two := callAs(r, "user-ok", "PUT", controllersPath(sid, "pc"), `{"controller_user_ids":["u-2","u-1"],"revision":4}`)

	if one.Code != 200 || two.Code != 200 {
		t.Fatalf("assign = %d %s / %d %s", one.Code, one.Body, two.Code, two.Body)
	}
	if got := controllersOf(t, r, sid, "orc"); !slices.Equal(got, []string{"u-2"}) {
		t.Errorf("orc controllers = %v", got)
	}
	if got := controllersOf(t, r, sid, "pc"); !slices.Equal(got, []string{"u-2", "u-1"}) {
		t.Errorf("pc controllers = %v, want a user on several combatants and several controllers on one", got)
	}
	if rev := gmRevision(t, r, sid); rev != 5 {
		t.Errorf("revision = %v, want 5 after two writes", rev)
	}
}

func TestAssignedControllerGetsOwnCombatantView(t *testing.T) {
	r, _, sid := viewTable(t)
	if mineFor(t, r, "user-two", sid, "orc") {
		t.Fatal("u-2 controls the orc before any assignment")
	}

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)

	if w.Code != 200 || !mineFor(t, r, "user-two", sid, "orc") {
		t.Fatalf("assign = %d %s; the player should now control the orc", w.Code, w.Body)
	}
}

func TestEmptyListClearsControllers(t *testing.T) {
	r, _, sid := viewTable(t)

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "pc"), `{"controller_user_ids":[],"revision":3}`)

	if w.Code != 200 || len(controllersOf(t, r, sid, "pc")) != 0 || mineFor(t, r, "user-two", sid, "pc") {
		t.Fatalf("clear = %d %s, controllers %v", w.Code, w.Body, controllersOf(t, r, sid, "pc"))
	}
}

func TestNonMemberAndObserverControllersAreRejectedAndNothingIsWritten(t *testing.T) {
	r, _, sid := viewTable(t)

	for name, ids := range map[string]string{"non-member": `["u-9"]`, "observer": `["u-3"]`, "valid then non-member": `["u-2","u-9"]`, "malformed id": `["not a user"]`} {
		w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":`+ids+`,"revision":3}`)

		if w.Code != http.StatusUnprocessableEntity || decodeBody(t, w)["code"] != "invalid" {
			t.Errorf("%s = %d %s, want 422 invalid", name, w.Code, w.Body)
		}
	}
	if rev := gmRevision(t, r, sid); rev != 3 || len(controllersOf(t, r, sid, "orc")) != 0 {
		t.Errorf("revision %v, orc controllers %v: a rejected assignment wrote something", rev, controllersOf(t, r, sid, "orc"))
	}
}

func TestOnlyTheGMSideMayAssign(t *testing.T) {
	r, _, sid := viewTable(t)

	for _, token := range []string{"user-two", "user-three"} {
		w := callAs(r, token, "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s, want 403", token, w.Code, w.Body)
		}
	}
	if w := callAs(r, "user-ok", "PUT", "/sessions/"+foreignSession+"/encounters/"+encE1+"/combatants/orc/controllers", `{"controller_user_ids":[],"revision":3}`); w.Code != http.StatusForbidden {
		t.Errorf("a session the GM does not run = %d, want 403", w.Code)
	}
}

func TestStaleRevisionIs409WithTheCurrentView(t *testing.T) {
	r, _, sid := viewTable(t)

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":2}`)

	b := decodeBody(t, w)
	if cur, _ := b["current"].(map[string]any); w.Code != http.StatusConflict || b["code"] != "revision_conflict" || cur["revision"] != float64(3) {
		t.Fatalf("stale = %d %s, want 409 revision_conflict with the current view", w.Code, w.Body)
	}
}

func TestAssignmentInputErrors(t *testing.T) {
	r, _, sid := viewTable(t)
	tests := []struct {
		name, path, body string
		want             int
	}{
		{"unknown combatant", controllersPath(sid, "nope"), `{"controller_user_ids":[],"revision":3}`, 404},
		{"unknown encounter", encPath(sid, "5f00000000000000000000ff") + "/combatants/orc/controllers", `{"controller_user_ids":[],"revision":3}`, 404},
		{"list missing", controllersPath(sid, "orc"), `{"revision":3}`, 400},
		{"no revision", controllersPath(sid, "orc"), `{"controller_user_ids":[]}`, 400},
		{"unknown field", controllersPath(sid, "orc"), `{"controller_user_ids":[],"revision":3,"role":"gm"}`, 422},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := callAs(r, "user-ok", "PUT", tt.path, tt.body); w.Code != tt.want {
				t.Fatalf("= %d %s, want %d", w.Code, w.Body, tt.want)
			}
		})
	}
}

func TestIntegrationCredentialsCannotAssign(t *testing.T) {
	r, _, sid := viewTable(t)
	_, secret := issue(t, r, "user-ok", `["read","write"]`)

	if w := callAs(r, secret, "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":[],"revision":3}`); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Fatalf("credential = %d %s, want it refused", w.Code, w.Body)
	}
}

// joinAs has token request to join through an invite and the GM approve it with role.
func joinAs(t *testing.T, r http.Handler, sid, token, role string) {
	t.Helper()
	code := inviteCode(t, r, "user-ok", sid)
	jw := callAs(r, token, "POST", "/sessions/join", `{"code":"`+code+`"}`)
	if jw.Code != http.StatusAccepted {
		t.Fatalf("join = %d %s", jw.Code, jw.Body)
	}
	reqID := decodeBody(t, jw)["id"].(string)
	if aw := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/requests/"+reqID+"/approve", `{"role":"`+role+`"}`); aw.Code != 200 {
		t.Fatalf("approve = %d %s", aw.Code, aw.Body)
	}
}

func TestRemovingABlockingOrLeavingMemberScrubsTheirControls(t *testing.T) {
	steps := map[string]func(r http.Handler, sid string) *httptest.ResponseRecorder{
		"removed": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return (callAs(r, "user-ok", "DELETE", "/sessions/"+sid+"/members/u-2", ""))
		},
		"blocked": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return (callAs(r, "user-ok", "POST", "/sessions/"+sid+"/blocks", `{"user_id":"u-2"}`))
		},
		"left": func(r http.Handler, sid string) *httptest.ResponseRecorder {
			return (callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""))
		},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			r, _, sid := viewTable(t) // u-2 controls the pc
			before := gmRevision(t, r, sid)

			if res := step(r, sid); res.Code >= 300 {
				t.Fatalf("%s = %d %s", name, res.Code, res.Body)
			}

			if got := controllersOf(t, r, sid, "pc"); len(got) != 0 {
				t.Errorf("pc controllers after %s = %v, want none", name, got)
			}
			if rev := gmRevision(t, r, sid); rev <= before {
				t.Errorf("revision %v after the scrub, want it to advance past %v so clients re-read", rev, before)
			}
		})
	}
}

func TestAMemberWhoLeavesAndRejoinsDoesNotRegainControl(t *testing.T) {
	r, db, sid := viewTable(t)
	if !mineFor(t, r, "user-two", sid, "pc") {
		t.Fatal("setup: u-2 should control the pc")
	}
	if w := callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""); w.Code >= 300 {
		t.Fatalf("leave = %d %s", w.Code, w.Body)
	}
	// A scrub that failed would have left the id behind: put it back by hand.
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.Combatants[0].ControllerUserIDs = []string{"u-2"} })

	joinAs(t, r, sid, "user-two", "player")

	if mineFor(t, r, "user-two", sid, "pc") {
		t.Fatal("u-2 regained control of the pc by rejoining: a stale controller id came back into force")
	}
}

func TestAStaleControllerIDGrantsNothingToANonMemberOrObserver(t *testing.T) {
	r, db, sid := viewTable(t)
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.Combatants[2].ControllerUserIDs = []string{"u-3", "u-9"} })

	// u-3 is an observer listed as a controller; u-9 is no member.
	w := callAs(r, "user-three", "GET", encPath(sid, encE1)+"/combatants/orc", "")

	if mine, _ := decodeBody(t, w)["mine"].(bool); mine {
		t.Error("an observer listed as controller still counts as controlling")
	}
}

func TestDemotingAPlayerToObserverScrubsAndPromotingBackStartsClean(t *testing.T) {
	r, db, sid := viewTable(t)

	if w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"observer"}`); w.Code != 200 {
		t.Fatalf("demote = %d %s", w.Code, w.Body)
	}
	if got := controllersOf(t, r, sid, "pc"); len(got) != 0 {
		t.Errorf("pc controllers after demotion = %v, want none", got)
	}
	// A scrub that failed would have left the id behind: put it back by hand.
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.Combatants[0].ControllerUserIDs = []string{"u-2"} })
	if w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"player"}`); w.Code != 200 {
		t.Fatalf("promote = %d %s", w.Code, w.Body)
	}
	if mineFor(t, r, "user-two", sid, "pc") {
		t.Error("promoted back to player, u-2 controls the pc again")
	}
}

// failingEncounters makes Find on the encounters collection fail while fail is set, as a store
// outage would.
type failingEncounters struct {
	store.Database
	fail *atomic.Bool
}

func (d failingEncounters) Collection(name string) store.Collection {
	c := d.Database.Collection(name)
	if name == data.CollectionEncounters {
		return failingFind{c, d.fail}
	}
	return c
}

type failingFind struct {
	store.Collection
	fail *atomic.Bool
}

func (c failingFind) Find(ctx context.Context, f bson.D, o store.FindOptions, out any) error {
	if c.fail.Load() {
		return errors.New("store down")
	}
	return c.Collection.Find(ctx, f, o, out)
}

func roleOf(t *testing.T, r http.Handler, token, sid string) string {
	t.Helper()
	w := callAs(r, token, "GET", "/sessions/"+sid, "")
	if w.Code != 200 {
		return ""
	}
	role, _ := decodeBody(t, w)["role"].(string)
	return role
}

func TestAFailedPreAdmissionScrubRefusesAdmissionAndLeavesTheRequestPending(t *testing.T) {
	var fail atomic.Bool
	r, db, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database { return failingEncounters{d, &fail} })
	if w := callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""); w.Code >= 300 {
		t.Fatalf("leave = %d %s", w.Code, w.Body)
	}
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.Combatants[0].ControllerUserIDs = []string{"u-2"} })
	code := inviteCode(t, r, "user-ok", sid)
	jw := callAs(r, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`)
	reqID := decodeBody(t, jw)["id"].(string)

	fail.Store(true)
	w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/requests/"+reqID+"/approve", `{"role":"player"}`)

	if w.Code < 500 || roleOf(t, r, "user-two", sid) != "" {
		t.Fatalf("approve with the scrub failing = %d %s, role %q; want a server error and no membership", w.Code, w.Body, roleOf(t, r, "user-two", sid))
	}
	fail.Store(false)
	if w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/requests/"+reqID+"/approve", `{"role":"player"}`); w.Code != 200 {
		t.Fatalf("approve retry = %d %s: the request should still have been pending", w.Code, w.Body)
	}
	if mineFor(t, r, "user-two", sid, "pc") {
		t.Error("u-2 controls the pc after the retried admission")
	}
}

func TestAFailedPreAdmissionScrubRefusesPromotionFromObserver(t *testing.T) {
	var fail atomic.Bool
	r, _, sid := viewTableOn(t, "3", func(d *fake.DB) store.Database { return failingEncounters{d, &fail} })
	if w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"observer"}`); w.Code != 200 {
		t.Fatalf("demote = %d %s", w.Code, w.Body)
	}

	fail.Store(true)
	w := callAs(r, "user-ok", "PATCH", "/sessions/"+sid+"/members/u-2", `{"role":"player"}`)

	if w.Code < 500 || roleOf(t, r, "user-two", sid) != "observer" {
		t.Fatalf("promote with the scrub failing = %d %s, role %q; want a server error and still observer", w.Code, w.Body, roleOf(t, r, "user-two", sid))
	}
}

func TestACoGMAssignsAndCanBeAssigned(t *testing.T) {
	r, _, sid := viewTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")

	byCoGM := callAs(r, "user-four", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)
	asController := callAs(r, "user-ok", "PUT", controllersPath(sid, "pc"), `{"controller_user_ids":["u-4"],"revision":4}`)

	if byCoGM.Code != 200 || asController.Code != 200 || !slices.Equal(controllersOf(t, r, sid, "pc"), []string{"u-4"}) {
		t.Fatalf("co-GM assigns = %d %s; co-GM assigned = %d %s", byCoGM.Code, byCoGM.Body, asController.Code, asController.Body)
	}
}

func TestAMemberWhoLeftIsRejectedAsAController(t *testing.T) {
	r, _, sid := viewTable(t)
	if w := callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""); w.Code >= 300 {
		t.Fatalf("leave = %d %s", w.Code, w.Body)
	}

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":`+strconv.Itoa(int(gmRevision(t, r, sid)))+`}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("assigning a member who left = %d %s, want 422", w.Code, w.Body)
	}
}

func TestDuplicateControllersAreStoredOnce(t *testing.T) {
	r, _, sid := viewTable(t)

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2","u-2","u-2"],"revision":3}`)

	if w.Code != 200 || !slices.Equal(controllersOf(t, r, sid, "orc"), []string{"u-2"}) {
		t.Fatalf("duplicates = %d %s, controllers %v, want one u-2", w.Code, w.Body, controllersOf(t, r, sid, "orc"))
	}
}

func TestAnOversizedControllerListIs422(t *testing.T) {
	r, _, sid := viewTable(t)
	ids := make([]string, data.MaxCombatants+1)
	for i := range ids {
		ids[i] = "u-" + strconv.Itoa(i+100)
	}
	body, _ := json.Marshal(map[string]any{"controller_user_ids": ids, "revision": 3})

	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), string(body))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d controllers = %d %s, want 422", len(ids), w.Code, w.Body)
	}
}

// flakyMembers fails FindOne on session_requests once while failOnce is set, and answers "not
// found" for u-2's member record from its second lookup on while leaveAfterFirst is set, as if
// the user left between the membership check and the write.
type flakyDB struct {
	store.Database
	failRequestOnce, leaveAfterFirst *atomic.Bool
	u2Lookups                        *atomic.Int64
}

func (d flakyDB) Collection(name string) store.Collection {
	c := d.Database.Collection(name)
	switch name {
	case data.CollectionSessionRequests:
		return flakyFindOne{c, func(bson.D) error {
			if d.failRequestOnce.CompareAndSwap(true, false) {
				return errors.New("timeout")
			}
			return nil
		}}
	case data.CollectionSessionMembers:
		return flakyFindOne{c, func(f bson.D) error {
			for _, e := range f {
				if e.Key == "user_id" && e.Value == "u-2" && d.leaveAfterFirst.Load() && d.u2Lookups.Add(1) > 1 {
					return store.ErrNotFound
				}
			}
			return nil
		}}
	}
	return c
}

type flakyFindOne struct {
	store.Collection
	gate func(bson.D) error
}

func (c flakyFindOne) FindOne(ctx context.Context, f bson.D, out any) error {
	if err := c.gate(f); err != nil {
		return err
	}
	return c.Collection.FindOne(ctx, f, out)
}

func newFlaky(t *testing.T) (http.Handler, *fake.DB, string, flakyDB) {
	t.Helper()
	d := flakyDB{failRequestOnce: new(atomic.Bool), leaveAfterFirst: new(atomic.Bool), u2Lookups: new(atomic.Int64)}
	r, db, sid := viewTableOn(t, "3", func(f *fake.DB) store.Database { d.Database = f; return d })
	return r, db, sid, d
}

func TestATransientPendingLookupErrorRefusesAdmissionAndARetrySucceeds(t *testing.T) {
	r, db, sid, d := newFlaky(t)
	if w := callAs(r, "user-two", "POST", "/sessions/"+sid+"/leave", ""); w.Code >= 300 {
		t.Fatalf("leave = %d %s", w.Code, w.Body)
	}
	replaceEncounter(t, db, encE1, sid, func(e *data.Encounter) { e.Combatants[0].ControllerUserIDs = []string{"u-2"} })
	code := inviteCode(t, r, "user-ok", sid)
	reqID := decodeBody(t, callAs(r, "user-two", "POST", "/sessions/join", `{"code":"`+code+`"}`))["id"].(string)

	d.failRequestOnce.Store(true)
	w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/requests/"+reqID+"/approve", `{"role":"player"}`)

	if w.Code < 500 || roleOf(t, r, "user-two", sid) != "" {
		t.Fatalf("approve with a failing lookup = %d %s, role %q; want a server error and no membership", w.Code, w.Body, roleOf(t, r, "user-two", sid))
	}
	if w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/requests/"+reqID+"/approve", `{"role":"player"}`); w.Code != 200 {
		t.Fatalf("retry = %d %s", w.Code, w.Body)
	}
	if mineFor(t, r, "user-two", sid, "pc") {
		t.Error("u-2 controls the pc after the retried admission")
	}
}

func TestAControllerWhoLeavesDuringAnAssignmentIsScrubbedAndTheAnswerIs409(t *testing.T) {
	r, _, sid, d := newFlaky(t)

	d.leaveAfterFirst.Store(true) // the first u-2 lookup (the membership check) passes; the re-check after the write does not
	w := callAs(r, "user-ok", "PUT", controllersPath(sid, "orc"), `{"controller_user_ids":["u-2"],"revision":3}`)
	d.leaveAfterFirst.Store(false)

	if w.Code != http.StatusConflict {
		t.Fatalf("assign while the member leaves = %d %s, want 409", w.Code, w.Body)
	}
	for _, cid := range []string{"orc", "pc"} {
		if got := controllersOf(t, r, sid, cid); slices.Contains(got, "u-2") {
			t.Errorf("%s still lists u-2 after the scrub: %v", cid, got)
		}
	}
}
