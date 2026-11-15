package domain

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

const viewSession = "5f0000000000000000000001"

// roleChecker answers relation checks from a map of user to role, as OpenFGA would: can_edit is
// gm or co_gm, can_view is any member, player and observer are the direct relations.
type roleChecker map[string]string

func (r roleChecker) Check(_ context.Context, user, _, _, relation string) (bool, error) {
	role := r[user]
	switch relation {
	case "can_view":
		return role != "", nil
	case "can_edit":
		return role == "gm" || role == "co_gm", nil
	default:
		return role == relation, nil
	}
}

// viewTable is a session with a GM, a co-GM, two players and an observer, built through the data
// layer with a checker that answers like OpenFGA would.
func viewTable(t *testing.T) (*data.Sessions, context.Context) {
	t.Helper()
	db := fake.NewDB()
	repo := data.NewSessions(db)
	ctx := data.WithGate(context.Background(), roleChecker{"gm1": "gm", "co1": "co_gm", "p1": "player", "p2": "player", "obs": "observer"}, "tok")
	if err := repo.Open(ctx, "gm1", viewSession, &data.Session{Name: "Friday"}); err != nil {
		t.Fatal(err)
	}
	d, err := data.Authorize(ctx, "gm1", viewSession, "can_edit")
	if err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"co1": data.RoleCoGM, "p1": data.RolePlayer, "p2": data.RolePlayer, "obs": data.RoleObserver} {
		if _, err := repo.AddMember(ctx, d, user, role); err != nil {
			t.Fatalf("add %s: %v", user, err)
		}
	}
	return repo, ctx
}

func viewerFor(t *testing.T, user string) data.Viewer {
	t.Helper()
	repo, ctx := viewTable(t)
	v, err := repo.AuthorizeView(ctx, user, viewSession)
	if err != nil {
		t.Fatalf("AuthorizeView(%s): %v", user, err)
	}
	return v
}

func two(n int) *int { return &n }

// secretEncounter has a hidden adversary that is also the current turn, a removed one, a status
// adversary, a player character with hidden-by-default health, and notes of both kinds.
func secretEncounter() (data.Encounter, []data.TrackerField) {
	init := 12.0
	enc := data.Encounter{
		Meta:               data.Meta{ID: "e1", OwnerUserID: "gm1", Revision: 7},
		Name:               "Ambush",
		GameSystemID:       "d20",
		Ordering:           "auto",
		SessionID:          viewSession,
		Status:             data.StatusActive,
		Round:              2,
		CurrentCombatantID: "c-hidden",
		TurnQueue:          []string{"c-hidden", "c-pc", "c-orc", "c-obj"},
		Combatants: []data.Combatant{
			{ID: "c-pc", Name: "Aria", Kind: data.KindPC, Initiative: &init, ControllerUserIDs: []string{"p1"},
				Health: map[string]int{"current": 9, "maximum": 20}, Notes: "Aria's shared note", NotesShared: true,
				Conditions: []data.Condition{{Name: "prone", RoundsRemaining: two(2)}}},
			{ID: "c-hidden", Name: "Assassin Vex", Kind: data.KindAdversary, Hidden: true, Initiative: &init,
				Health: map[string]int{"current": 31, "maximum": 31}, Notes: "SECRET plan", HealthVisibility: data.HealthFull},
			{ID: "c-orc", Name: "Orc", Kind: data.KindAdversary, HealthVisibility: data.HealthStatus, Notes: "GM only orc note",
				Health: map[string]int{"current": 5, "maximum": 20}},
			{ID: "c-obj", Name: "Door", Kind: data.KindObject, Health: map[string]int{"current": 3, "maximum": 3}},
			{ID: "c-gone", Name: "Fled goblin", Kind: data.KindAdversary, Removed: true},
		},
	}
	def := []data.TrackerField{
		{Key: "current", Kind: data.FieldCounter, Min: two(0), MaxRef: "maximum"},
		{Key: "maximum", Kind: data.FieldCounter, Min: two(1)},
	}
	return enc, def
}

func mustView(t *testing.T, user string) EncounterView {
	t.Helper()
	enc, def := secretEncounter()
	v, err := BuildView(enc, def, viewerFor(t, user))
	if err != nil {
		t.Fatalf("BuildView(%s): %v", user, err)
	}
	return v
}

func viewIDs(view EncounterView) []string {
	var out []string
	for _, c := range view["combatants"].([]map[string]any) {
		out = append(out, c["id"].(string))
	}
	return out
}

func combatantOf(t *testing.T, view EncounterView, id string) map[string]any {
	t.Helper()
	c, ok := view.Combatant(id)
	if !ok {
		t.Fatalf("combatant %s not in view %v", id, viewIDs(view))
	}
	return c
}

func TestGMAndCoGMSeeEverything(t *testing.T) {
	for _, user := range []string{"gm1", "co1"} {
		view := mustView(t, user)

		if got := viewIDs(view); !slices.Equal(got, []string{"c-pc", "c-hidden", "c-orc", "c-obj", "c-gone"}) {
			t.Errorf("%s combatants = %v, want all five", user, got)
		}
		hidden := combatantOf(t, view, "c-hidden")
		if hidden["notes"] != "SECRET plan" || hidden["hidden"] != true || hidden["initiative"] == nil {
			t.Errorf("%s hidden combatant = %v, want notes, hidden flag, initiative", user, hidden)
		}
		if view["revision"] != int64(7) || view["owner_user_id"] != "gm1" || view["current_combatant_id"] != "c-hidden" {
			t.Errorf("%s encounter fields = %v", user, view)
		}
		if view["combatant_count"] != 4 || len(view["turn_queue"].([]string)) != 4 {
			t.Errorf("%s count=%v queue=%v, want 4 (removed ones do not count)", user, view["combatant_count"], view["turn_queue"])
		}
		if _, tagged := view[FieldViewTag]; tagged {
			t.Errorf("%s view has a view_tag; a GM sees the real revision", user)
		}
	}
}

func TestPlayerSeesOnlyVisibleCombatantsAndAHiddenTurnBelongsToNoOne(t *testing.T) {
	view := mustView(t, "p2")

	if got := viewIDs(view); !slices.Equal(got, []string{"c-pc", "c-orc", "c-obj"}) {
		t.Errorf("combatants = %v, want the hidden and removed ones absent", got)
	}
	if got := view["turn_queue"].([]string); !slices.Equal(got, []string{"c-pc", "c-orc", "c-obj"}) {
		t.Errorf("turn_queue = %v, want only visible combatants", got)
	}
	if _, has := view["current_combatant_id"]; has {
		t.Errorf("current_combatant_id = %v, want absent while a hidden combatant is current", view["current_combatant_id"])
	}
	if view["combatant_count"] != 3 {
		t.Errorf("combatant_count = %v, want 3 visible", view["combatant_count"])
	}
	if view["round"] != 2 || view["name"] != "Ambush" || view["status"] != data.StatusActive {
		t.Errorf("view = %v, want name, status, round", view)
	}
}

func TestPlayerViewLeaksNothingOfTheHiddenCombatant(t *testing.T) {
	for _, user := range []string{"p1", "p2", "obs"} {
		b, err := json.Marshal(mustView(t, user))
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		for _, secret := range []string{"c-hidden", "Assassin", "Vex", "SECRET", "c-gone", "Fled goblin", "GM only orc note", "31"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s view contains %q: %s", user, secret, body)
			}
		}
	}
}

func TestNonGMViewHasNoOwnerAuditRevisionOrSettings(t *testing.T) {
	view := mustView(t, "p2")

	for _, key := range []string{"revision", "owner_user_id", "created_at", "updated_at", "created_by", "updated_by", "ordering", "tie_breaker",
		"position", "theme_id", "labels", "colorblind", "inactive_at", "locked_at", "deleted_at"} {
		if _, has := view[key]; has {
			t.Errorf("player view has %q", key)
		}
	}
	if view[FieldViewTag] == nil {
		t.Error("player view has no view_tag")
	}
}

func TestHealthIsShownPerCombatantSetting(t *testing.T) {
	player := mustView(t, "p2")

	pc := combatantOf(t, player, "c-pc") // hidden-by-default health, controlled by p1
	orc := combatantOf(t, player, "c-orc")
	door := combatantOf(t, player, "c-obj")

	if pc["health"] != nil || pc["health_status"] != nil {
		t.Errorf("another player's character shows health %v / %v, want none (default hidden)", pc["health"], pc["health_status"])
	}
	if orc["health"] != nil || orc["health_status"] != StatusNearDeath {
		t.Errorf("status-visibility orc = health %v status %v, want status near_death and no numbers", orc["health"], orc["health_status"])
	}
	if door["health"] != nil || door["health_status"] != nil {
		t.Errorf("default-visibility door shows health %v / %v, want none", door["health"], door["health_status"])
	}
}

func TestAControllersOwnCombatantAlwaysShowsFullHealth(t *testing.T) {
	own := combatantOf(t, mustView(t, "p1"), "c-pc")
	other := combatantOf(t, mustView(t, "p2"), "c-pc")

	if !reflect.DeepEqual(own["health"], map[string]int{"current": 9, "maximum": 20}) || own["mine"] != true {
		t.Errorf("controller sees health=%v mine=%v, want full health and mine", own["health"], own["mine"])
	}
	if other["mine"] != false || other["health"] != nil {
		t.Errorf("non-controller sees health=%v mine=%v", other["health"], other["mine"])
	}
}

func TestObserverIsAPlayerWithNoControlAndNoMineFlag(t *testing.T) {
	obs := mustView(t, "obs")

	pc := combatantOf(t, obs, "c-pc")

	if _, has := pc["mine"]; has {
		t.Errorf("observer view has a mine flag: %v", pc)
	}
	if pc["health"] != nil {
		t.Errorf("observer sees health %v of a combatant it does not control", pc["health"])
	}
	if got := viewIDs(obs); !slices.Equal(got, viewIDs(mustView(t, "p2"))) {
		t.Errorf("observer combatants = %v, want the same as a player's", got)
	}
}

func TestGMOnlyNotesAreNeverSentToNonGMs(t *testing.T) {
	for _, user := range []string{"p1", "obs"} {
		view := mustView(t, user)
		orc, pc := combatantOf(t, view, "c-orc"), combatantOf(t, view, "c-pc")

		if _, has := orc["notes"]; has {
			t.Errorf("%s sees GM-only notes: %v", user, orc["notes"])
		}
		if pc["notes"] != "Aria's shared note" {
			t.Errorf("%s notes on a shared-notes combatant = %v", user, pc["notes"])
		}
	}
}

func TestRevisionAndTagsDoNotMoveWhenOnlyHiddenDataChanges(t *testing.T) {
	enc, def := secretEncounter()
	player := viewerFor(t, "p2")
	before, _ := BuildView(enc, def, player)

	enc.Revision++
	enc.UpdatedAt = enc.UpdatedAt.Add(time.Hour)
	enc.Combatants[1].Name, enc.Combatants[1].Notes = "Renamed", "other"
	enc.Combatants[1].Health = map[string]int{"current": 1, "maximum": 31}
	enc.Combatants = append(enc.Combatants, data.Combatant{ID: "c-new-hidden", Name: "Another", Kind: data.KindAdversary, Hidden: true})
	after, _ := BuildView(enc, def, player)

	if before[FieldViewTag] != after[FieldViewTag] || !reflect.DeepEqual(before, after) {
		t.Fatalf("view changed after a hidden-only edit:\nbefore %v\nafter  %v", before, after)
	}
	enc.Combatants[0].Conditions = nil
	changed, _ := BuildView(enc, def, player)
	if changed[FieldViewTag] == before[FieldViewTag] {
		t.Error("view_tag did not change after a visible edit")
	}
}

func TestZeroViewerAndAMismatchedSessionGetNoView(t *testing.T) {
	enc, def := secretEncounter()
	other := enc
	other.SessionID = "5f00000000000000000000ff"

	_, errZero := BuildView(enc, def, data.Viewer{})
	_, errOther := BuildView(other, def, viewerFor(t, "p2"))

	if errZero == nil || errOther == nil {
		t.Fatalf("zero viewer err=%v, other-session err=%v; want both refused", errZero, errOther)
	}
}

func TestCombatantIDsAreRandomNotObjectIDs(t *testing.T) {
	prefixes := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := randomID()
		if len(id) != 24 {
			t.Fatalf("id %q is not 24 hex characters", id)
		}
		prefixes[id[:8]] = true
	}
	if len(prefixes) < 10 { // an ObjectID's first four bytes are the clock
		t.Fatalf("only %d distinct 8-character prefixes in 20 ids; ids look time-ordered", len(prefixes))
	}
}

func TestHealthStatusBands(t *testing.T) {
	def := []data.TrackerField{{Key: "current", Kind: data.FieldCounter, Min: two(0), MaxRef: "maximum"}, {Key: "maximum", Kind: data.FieldCounter}}
	for _, tc := range []struct {
		cur, max int
		want     string
	}{{20, 20, StatusHealthy}, {19, 20, StatusHurt}, {6, 20, StatusHurt}, {5, 20, StatusNearDeath}, {1, 20, StatusNearDeath}, {0, 20, StatusDown}, {-3, 20, StatusDown}, {30, 20, StatusHealthy}, {0, 0, StatusDown}} {
		got, ok := healthStatus(def, map[string]int{"current": tc.cur, "maximum": tc.max})
		if !ok || got != tc.want {
			t.Errorf("%d/%d = %q, want %q", tc.cur, tc.max, got, tc.want)
		}
	}
	simple := []data.TrackerField{{Key: "health", Kind: data.FieldCounter, Min: two(0)}}
	if s, _ := healthStatus(simple, map[string]int{"health": 4}); s != StatusHealthy {
		t.Errorf("unbounded counter above its minimum = %q", s)
	}
	if s, _ := healthStatus(simple, map[string]int{"health": 0}); s != StatusDown {
		t.Errorf("unbounded counter at its minimum = %q", s)
	}
	if _, ok := healthStatus([]data.TrackerField{{Key: "dead", Kind: data.FieldFlag}}, nil); ok {
		t.Error("a definition with no counter produced a status")
	}
}

// fieldsOf lists the JSON names of a struct type's serialized fields, flattening embedded structs.
func fieldsOf(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, fieldsOf(f.Type)...)
			continue
		}
		if name, _, ok := jsonName(f); ok {
			out = append(out, name)
		}
	}
	return out
}

func TestEveryDataFieldHasAVisibilityRule(t *testing.T) {
	for _, tc := range []struct {
		typ   reflect.Type
		rules map[string]data.ViewLevel
	}{
		{reflect.TypeOf(data.Encounter{}), encounterRules},
		{reflect.TypeOf(data.Combatant{}), combatantRules},
		{reflect.TypeOf(data.Condition{}), conditionRules},
	} {
		fields := fieldsOf(tc.typ)
		for _, f := range fields {
			if _, ruled := tc.rules[f]; !ruled {
				t.Errorf("%s.%s has no visibility rule: add it to the tables in view.go (it is hidden from everyone until you do)", tc.typ.Name(), f)
			}
		}
		for f := range tc.rules {
			if !slices.Contains(fields, f) {
				t.Errorf("rule for %q matches no field of %s", f, tc.typ.Name())
			}
		}
	}
}

func TestAFieldWithoutARuleIsNeverProjected(t *testing.T) {
	type sample struct {
		Shown   string `json:"shown"`
		Unruled string `json:"unruled"`
		GMOnly  string `json:"gm_only"`
	}
	rules := map[string]data.ViewLevel{"shown": data.ViewObserver, "gm_only": data.ViewGM}
	s := sample{"a", "b", "c"}

	player := projectStruct(reflect.ValueOf(s), rules, data.ViewPlayer, nil, nil)
	gm := projectStruct(reflect.ValueOf(s), rules, data.ViewGM, nil, nil)

	if !reflect.DeepEqual(player, map[string]any{"shown": "a"}) || !reflect.DeepEqual(gm, map[string]any{"shown": "a", "gm_only": "c"}) {
		t.Fatalf("player=%v gm=%v; an unruled field must appear for no one", player, gm)
	}
}

func TestAHiddenCombatantIsVisibleToItsControllerOnly(t *testing.T) {
	enc, def := secretEncounter()
	enc.Combatants[1].ControllerUserIDs = []string{"p1", "obs"}
	p1, _ := BuildView(enc, def, viewerFor(t, "p1"))
	p2, _ := BuildView(enc, def, viewerFor(t, "p2"))
	obs, _ := BuildView(enc, def, viewerFor(t, "obs"))

	if _, ok := p1.Combatant("c-hidden"); !ok {
		t.Error("the controller does not see its hidden combatant")
	}
	for name, v := range map[string]EncounterView{"p2": p2, "obs": obs} {
		if _, ok := v.Combatant("c-hidden"); ok {
			t.Errorf("%s sees a hidden combatant it does not control (an observer controls nothing)", name)
		}
	}
	if p1["current_combatant_id"] != "c-hidden" || !slices.Contains(p1["turn_queue"].([]string), "c-hidden") {
		t.Errorf("controller view: current=%v queue=%v, want the controlled hidden combatant in both", p1["current_combatant_id"], p1["turn_queue"])
	}
}

func TestAuthorizeViewRefusesAMemberWhoseRecordAndTuplesDisagree(t *testing.T) {
	repo, ctx := viewTable(t)
	// The record says player; OpenFGA (the checker) says this user is only an observer.
	d, _ := data.Authorize(ctx, "gm1", viewSession, "can_edit")
	if _, err := repo.AddMember(ctx, d, "drift", data.RolePlayer); err != nil {
		t.Fatal(err)
	}
	ctx = data.WithGate(ctx, roleChecker{"drift": "observer", "gm1": "gm"}, "tok")

	v, err := repo.AuthorizeView(ctx, "drift", viewSession)

	if err == nil || v.Level() != data.ViewNone {
		t.Fatalf("viewer=%v err=%v, want ErrForbidden and the zero viewer", v, err)
	}
}

func TestAuthorizeViewRefusesANonMember(t *testing.T) {
	repo, ctx := viewTable(t)

	v, err := repo.AuthorizeView(ctx, "stranger", viewSession)

	if err == nil || v.Level() != data.ViewNone {
		t.Fatalf("viewer=%v err=%v, want ErrForbidden", v, err)
	}
}

func TestAuthorizeViewMapsEachRoleToItsLevel(t *testing.T) {
	for user, want := range map[string]data.ViewLevel{"gm1": data.ViewGM, "co1": data.ViewGM, "p1": data.ViewPlayer, "obs": data.ViewObserver} {
		if got := viewerFor(t, user).Level(); got != want {
			t.Errorf("%s level = %v, want %v", user, got, want)
		}
	}
}
