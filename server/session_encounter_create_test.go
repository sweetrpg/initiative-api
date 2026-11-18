package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"

	"github.com/sweetrpg/initiative-api/data"
	"go.mongodb.org/mongo-driver/bson"
)

const newSessionEncounter = `{"name":"Ambush","game_system_id":"` + data.DefaultSystemID + `"}`

func createIn(r http.Handler, token, sid string) *httptest.ResponseRecorder {
	return callAs(r, token, "POST", "/sessions/"+sid+"/encounters", newSessionEncounter)
}

func sessionRevision(t *testing.T, r http.Handler, sid string) string {
	t.Helper()
	return strconv.Itoa(int(decodeBody(t, callAs(r, "user-ok", "GET", "/sessions/"+sid, ""))["revision"].(float64)))
}

// setCoGMCreate has token PATCH the co-GM encounter-creation setting at the session's current revision.
func setCoGMCreate(t *testing.T, r http.Handler, token, sid string, on bool) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(r, token, "PATCH", "/sessions/"+sid, `{"allow_co_gm_create_encounter":`+strconv.FormatBool(on)+`,"revision":`+sessionRevision(t, r, sid)+`}`)
}

func TestTheGMCreatesAnEncounterInTheSessionAndOwnsIt(t *testing.T) {
	r, db, sid := viewTable(t)
	seedSystem(t, db)

	w := createIn(r, "user-ok", sid)

	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	e := storedEncounter(t, db, decodeBody(t, w)["id"].(string))
	if e.SessionID != sid || e.OwnerUserID != "u-1" {
		t.Errorf("stored session %q owner %q, want the session and the GM", e.SessionID, e.OwnerUserID)
	}
}

func TestACoGMCreatesOnlyWhileTheGMAllowsItAndLosesItAtOnce(t *testing.T) {
	r, db, sid := viewTable(t)
	seedSystem(t, db)
	joinAs(t, r, sid, "user-four", "co_gm")

	off := createIn(r, "user-four", sid)
	if on := setCoGMCreate(t, r, "user-ok", sid, true); on.Code != http.StatusOK {
		t.Fatalf("GM enabling = %d %s", on.Code, on.Body)
	}
	allowed := createIn(r, "user-four", sid)
	if again := setCoGMCreate(t, r, "user-ok", sid, false); again.Code != http.StatusOK {
		t.Fatalf("GM disabling = %d %s", again.Code, again.Body)
	}
	revoked := createIn(r, "user-four", sid)

	if off.Code != http.StatusForbidden || revoked.Code != http.StatusForbidden {
		t.Errorf("setting off = %d, after turning it off again = %d, want 403 both", off.Code, revoked.Code)
	}
	if allowed.Code != http.StatusCreated {
		t.Fatalf("co-GM with the setting on = %d %s", allowed.Code, allowed.Body)
	}
	if e := storedEncounter(t, db, decodeBody(t, allowed)["id"].(string)); e.OwnerUserID != "u-1" || e.SessionID != sid {
		t.Errorf("co-GM's encounter owner %q session %q, want the GM and the session", e.OwnerUserID, e.SessionID)
	}
}

func TestOnlyTheGMChangesTheSettingAndOnlyTheGMAndCoGMsSeeIt(t *testing.T) {
	r, _, sid := viewTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")

	byCoGM := setCoGMCreate(t, r, "user-four", sid, true)
	byPlayer := setCoGMCreate(t, r, "user-two", sid, true)
	byGM := setCoGMCreate(t, r, "user-ok", sid, true)
	unknown := callAs(r, "user-ok", "PATCH", "/sessions/"+sid, `{"revision":1,"bogus":true}`)

	if byCoGM.Code != http.StatusForbidden || byPlayer.Code != http.StatusForbidden {
		t.Errorf("co-GM = %d, player = %d, want 403 both", byCoGM.Code, byPlayer.Code)
	}
	if byGM.Code != http.StatusOK || decodeBody(t, byGM)["allow_co_gm_create_encounter"] != true {
		t.Errorf("GM = %d %s, want 200 with the setting on", byGM.Code, byGM.Body)
	}
	if unknown.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown field = %d, want 422", unknown.Code)
	}
	for token, want := range map[string]bool{"user-ok": true, "user-four": true, "user-two": false, "user-three": false} {
		_, has := decodeBody(t, callAs(r, token, "GET", "/sessions/"+sid, ""))["allow_co_gm_create_encounter"]
		if has != want {
			t.Errorf("%s sees the setting = %v, want %v", token, has, want)
		}
	}
}

func TestCreatingASessionCanTurnTheSettingOn(t *testing.T) {
	r, _, _ := viewTable(t)

	w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"Open","allow_co_gm_create_encounter":true}`)

	if w.Code != http.StatusCreated || decodeBody(t, w)["allow_co_gm_create_encounter"] != true {
		t.Fatalf("create = %d %s, want 201 with the setting on", w.Code, w.Body)
	}
}

func TestPlayersAndObserversCannotCreateEvenWithTheSettingOn(t *testing.T) {
	r, db, sid := viewTable(t)
	setCoGMCreate(t, r, "user-ok", sid, true)

	player, observer := createIn(r, "user-two", sid), createIn(r, "user-three", sid)

	if player.Code != http.StatusForbidden || observer.Code != http.StatusForbidden {
		t.Errorf("player = %d, observer = %d, want 403 both", player.Code, observer.Code)
	}
	if n, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), nil); n != 1 {
		t.Errorf("%d encounters stored, want only the fixture's", n)
	}
}

func TestSessionEncountersCountAgainstTheGMsLimitForGMAndCoGMAlike(t *testing.T) {
	r, db, sid := viewTableLimit(t, "2") // the fixture's session encounter is the first
	seedSystem(t, db)
	joinAs(t, r, sid, "user-four", "co_gm")
	setCoGMCreate(t, r, "user-ok", sid, true)

	second := createIn(r, "user-ok", sid)
	third := createIn(r, "user-four", sid)

	if second.Code != http.StatusCreated {
		t.Fatalf("second = %d %s", second.Code, second.Body)
	}
	if b := decodeBody(t, third); third.Code != http.StatusForbidden || b["code"] != "entitlement_limit_reached" {
		t.Errorf("co-GM over the GM's limit = %d %s, want 403 entitlement_limit_reached", third.Code, third.Body)
	}
}

func TestACoGMCannotCreateWithoutTheGMsRecordedEntitlements(t *testing.T) {
	r, db, sid := viewTable(t)
	joinAs(t, r, sid, "user-four", "co_gm")
	setCoGMCreate(t, r, "user-ok", sid, true)
	var sess data.Session
	if err := db.Fake(data.CollectionSessions).FindOneAndUpdate(t.Context(), bson.D{{Key: "_id", Value: sid}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "owner_limits", Value: ""}}}}, &sess); err != nil {
		t.Fatal(err)
	}

	w := createIn(r, "user-four", sid)

	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "owner_entitlements_pending" {
		t.Errorf("co-GM with no recorded entitlements = %d %s, want 503 owner_entitlements_pending", w.Code, w.Body)
	}
}

func TestCreatingInAnotherSessionOrWithACredentialIsRefused(t *testing.T) {
	r, db, sid := viewTable(t)
	_, secret := issue(t, r, "user-ok", `["read","write"]`)

	foreign := createIn(r, "user-ok", foreignSession)
	cred := createIn(r, secret, sid)

	if foreign.Code != http.StatusForbidden {
		t.Errorf("a session the caller does not belong to = %d, want 403", foreign.Code)
	}
	patch := callAs(r, secret, "PATCH", "/sessions/"+sid, `{"name":"x","revision":1}`)
	if cred.Code != http.StatusForbidden || patch.Code != http.StatusForbidden {
		t.Errorf("integration credential create = %d, patch = %d, want 403 both", cred.Code, patch.Code)
	}
	if n, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), nil); n != 1 {
		t.Errorf("%d encounters stored, want only the fixture's", n)
	}
}

func TestPatchingASessionValidatesAfterAuthorizingAndNeedsAChange(t *testing.T) {
	r, _, sid := viewTable(t)
	rev := sessionRevision(t, r, sid)

	empty := callAs(r, "user-ok", "PATCH", "/sessions/"+sid, `{"revision":`+rev+`}`)
	stale := callAs(r, "user-ok", "PATCH", "/sessions/"+sid, `{"name":"x","revision":99}`)
	outsider := callAs(r, "user-ok", "PATCH", "/sessions/"+foreignSession, `{"name":"","revision":1}`)

	if empty.Code != http.StatusUnprocessableEntity || decodeBody(t, empty)["message"] != "nothing to update" {
		t.Errorf("empty patch = %d %s, want 422 nothing to update", empty.Code, empty.Body)
	}
	if stale.Code != http.StatusConflict {
		t.Errorf("stale revision = %d, want 409", stale.Code)
	}
	if outsider.Code != http.StatusForbidden {
		t.Errorf("a session the caller does not belong to, with a bad name = %d, want 403 before validation", outsider.Code)
	}
}

// gatedTable is a session over an entitlements-api that answers the game-systems flag per bearer
// (gmHas for user-ok, coHas for user-four) and every limit as limit, with a gated system seeded and
// a co-GM allowed to create.
func gatedTable(t *testing.T, gmHas, coHas bool, limit int) (r http.Handler, db *fake.DB, sid string) {
	t.Helper()
	return gatedTableOn(t, gmHas, coHas, limit, func(d *fake.DB) store.Database { return d })
}

// gatedTableOn is gatedTable with the router's database wrapped by wrap; the returned db is the
// underlying fake.
func gatedTableOn(t *testing.T, gmHas, coHas bool, limit int, wrap func(*fake.DB) store.Database) (r http.Handler, db *fake.DB, sid string) {
	t.Helper()
	db = fake.NewDB()
	seedSystem(t, db)
	gated := data.GameSystem{ID: "dnd5e", Name: "D&D 5e", Family: "d20", Gated: true,
		Fields: []data.TrackerField{{Key: "hp", Label: "HP", Kind: data.FieldCounter, Def: 10}}}
	if _, err := data.NewSystemRepo(db).Upsert(t.Context(), gated); err != nil {
		t.Fatal(err)
	}
	ent := func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("key") == domain.KeyGameSystemsExtra {
			has := map[string]bool{"Bearer user-ok": gmHas, "Bearer user-four": coHas}[req.Header.Get("Authorization")]
			_, _ = w.Write([]byte(`{"key":"` + domain.KeyGameSystemsExtra + `","type":"flag","value":` + strconv.FormatBool(has) + `,"ttl_seconds":0}`))
			return
		}
		value := limit
		if req.URL.Query().Get("key") == domain.KeyMembersPerSession {
			value = 20 // these tests are about the saved-encounters limit, not the roster
		}
		_, _ = w.Write([]byte(`{"key":"` + req.URL.Query().Get("key") + `","type":"limit","value":` + strconv.Itoa(value) + `,"ttl_seconds":0}`))
	}
	withLimits := func(svc *Services) {
		svc.Limits = domain.NewLimits(domain.Kind{Name: "encounters", Key: domain.KeySavedEncounters, Repo: svc.Encounters.Items()})
	}
	r, _ = credRouterEnt(t, wrap(db), roleChecks, ent, withLimits)
	w := callAs(r, "user-ok", "POST", "/sessions", `{"name":"Friday","allow_co_gm_create_encounter":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", w.Code, w.Body)
	}
	sid = decodeBody(t, w)["id"].(string)
	joinAs(t, r, sid, "user-four", "co_gm")
	return r, db, sid
}

func gated(r http.Handler, token, sid string) *httptest.ResponseRecorder {
	return callAs(r, token, "POST", "/sessions/"+sid+"/encounters", `{"name":"G","game_system_id":"dnd5e"}`)
}

func TestACoGMsGatedSystemUseFollowsTheGMsFlagNotTheirOwn(t *testing.T) {
	cases := map[string]struct {
		gmHas, coHas bool
		want         int
	}{
		"co-GM holds the flag, the GM does not":     {false, true, http.StatusForbidden},
		"the GM holds the flag, the co-GM does not": {true, false, http.StatusCreated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, sid := gatedTable(t, c.gmHas, c.coHas, 5)

			if w := gated(r, "user-four", sid); w.Code != c.want {
				t.Errorf("co-GM gated create = %d %s, want %d", w.Code, w.Body, c.want)
			}
		})
	}
}

func TestACoGMCannotCreateOnAnOwnerRecordOlderThanTheBound(t *testing.T) {
	r, db, sid := gatedTable(t, true, true, 5)
	old := time.Now().Add(-8 * 24 * time.Hour)
	var sess data.Session
	if err := db.Fake(data.CollectionSessions).FindOneAndUpdate(t.Context(), bson.D{{Key: "_id", Value: sid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "owner_limits_at", Value: old}}}}, &sess); err != nil {
		t.Fatal(err)
	}

	w := callAs(r, "user-four", "POST", "/sessions/"+sid+"/encounters", newSessionEncounter)

	if w.Code != http.StatusServiceUnavailable || decodeBody(t, w)["code"] != "owner_entitlements_pending" {
		t.Errorf("co-GM on a stale record = %d %s, want 503 owner_entitlements_pending", w.Code, w.Body)
	}
}

func TestConcurrentCreatesNeverLeaveTheGMOverTheLimit(t *testing.T) {
	r, db, sid := gatedTable(t, true, true, 3)
	if w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/encounters", newSessionEncounter); w.Code != http.StatusCreated {
		t.Fatalf("the first, sequential create = %d %s, want 201 (the test must not pass with no creates at all)", w.Code, w.Body)
	}
	var created atomic.Int64
	created.Add(1) // the sequential create above
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/encounters", newSessionEncounter); w.Code == http.StatusCreated {
				created.Add(1)
			}
		}()
	}
	wg.Wait()

	live, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), bson.D{{Key: "deleted_at", Value: nil}})
	if live > 3 {
		t.Errorf("%d live encounters, want at most the limit of 3", live)
	}
	if created.Load() < live {
		t.Errorf("%d creates answered 201 but %d encounters are live", created.Load(), live)
	}
}

// overCountAfterCreate answers the post-create recount of the encounters collection with a number
// over any limit, as if concurrent creates had pushed the owner past it.
type overCountDB struct {
	store.Database
	armed *atomic.Int64
}

func (d overCountDB) Collection(name string) store.Collection {
	c := d.Database.Collection(name)
	if name == data.CollectionEncounters {
		return overCount{c, d.armed}
	}
	return c
}

type overCount struct {
	store.Collection
	armed *atomic.Int64
}

func (c overCount) CountDocuments(ctx context.Context, f bson.D) (int64, error) {
	if n := c.armed.Load(); n > 0 && c.armed.Add(1) > 2 { // the second count of an armed request
		return 99, nil
	}
	return c.Collection.CountDocuments(ctx, f)
}

func TestAnOverLimitCreateThatCannotBeRetractedIsAServerErrorNotALimitError(t *testing.T) {
	var armed atomic.Int64
	var failRetract atomic.Bool
	r, db, sid := gatedTableOn(t, true, true, 3, func(d *fake.DB) store.Database {
		return failingUpdates{overCountDB{d, &armed}, data.CollectionEncounters, func(u bson.D) error {
			if failRetract.Load() && setsKey(u, "deleted_at") {
				return errors.New("store down")
			}
			return nil
		}}
	})

	armed.Store(1)
	failRetract.Store(true)
	failed := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/encounters", newSessionEncounter)
	armed.Store(0)
	failRetract.Store(false)

	if failed.Code < 500 {
		t.Fatalf("an over-limit create whose retract failed = %d %s, want a server error: the encounter is still live", failed.Code, failed.Body)
	}
	live, _ := db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), bson.D{{Key: "deleted_at", Value: nil}})
	if live != 1 {
		t.Fatalf("%d live encounters, want the 1 the failed retract left behind", live)
	}

	armed.Store(1)
	retracted := callAs(r, "user-ok", "POST", "/sessions/"+sid+"/encounters", newSessionEncounter)
	armed.Store(0)
	live, _ = db.Fake(data.CollectionEncounters).CountDocuments(t.Context(), bson.D{{Key: "deleted_at", Value: nil}})
	if retracted.Code != http.StatusForbidden || live != 1 {
		t.Fatalf("an over-limit create that retracts cleanly = %d %s with %d live; want 403 and the earlier one only", retracted.Code, retracted.Body, live)
	}
}

func setsKey(u bson.D, key string) bool {
	for _, op := range u {
		if set, ok := op.Value.(bson.D); ok && op.Key == "$set" {
			for _, e := range set {
				if e.Key == key {
					return true
				}
			}
		}
	}
	return false
}
