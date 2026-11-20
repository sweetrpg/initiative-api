package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

type noticeTable struct {
	r       http.Handler
	db      *fake.DB
	sid     string
	hub     *stream.Hub
	ses     *domain.Sessions
	enc     *domain.Encounters
	relAPI  string
	service *Services
}

// newNoticeTable wires a hub as the change-request notifier and the preference store over the
// standard session fixture (GM u-1, player u-2 who controls pc, observer u-3).
func newNoticeTable(t *testing.T) noticeTable {
	t.Helper()
	n := noticeTable{}
	var captured *fake.DB
	routerHook = func(svc *Services) {
		n.hub = stream.New(svc.Encounters)
		n.ses, n.enc, n.service = svc.Sessions, svc.Encounters, svc
		svc.NotificationPrefs = domain.NewNotificationPrefs(captured, svc.Sessions)
		n.hub.SetPreferences(svc.NotificationPrefs)
		n.hub.SetManagers(svc.Sessions)
		svc.ChangeRequests.Notifier = n.hub
	}
	t.Cleanup(func() { routerHook = nil })
	n.r, n.db, n.sid = viewTableOn(t, "3", func(d *fake.DB) store.Database { captured = d; return d })
	seedSystem(t, n.db)
	rel := httptest.NewServer(http.HandlerFunc(roleChecks))
	t.Cleanup(rel.Close)
	n.relAPI = rel.URL
	return n
}

func (n noticeTable) viewer(t *testing.T, token string) data.Viewer {
	t.Helper()
	actor := map[string]string{"user-ok": "u-1", "user-two": "u-2", "user-three": "u-3"}[token]
	ctx := data.WithGate(context.Background(), relations.New(n.relAPI), token)
	v, err := n.ses.Viewer(ctx, actor, n.sid)
	if err != nil {
		t.Fatalf("viewer for %s: %v", token, err)
	}
	return v
}

func (n noticeTable) encounter(t *testing.T) data.Encounter {
	t.Helper()
	var enc data.Encounter
	if err := n.db.Fake(data.CollectionEncounters).FindOne(context.Background(), bson.D{{Key: "_id", Value: encE1}}, &enc); err != nil {
		t.Fatal(err)
	}
	return enc
}

// notices are the addressed events among evs.
func notices(evs []stream.Event) []stream.Event {
	var out []stream.Event
	for _, e := range evs {
		switch e.Type {
		case data.NoticeTurnYours, data.NoticeTurnNext, data.NoticeChangeSubmitted, data.NoticeChangeApproved, data.NoticeChangeRejected:
			out = append(out, e)
		}
	}
	return out
}

// withoutNotices drops the addressed events, leaving the encounter state events.
func withoutNotices(evs []stream.Event) []stream.Event {
	var out []stream.Event
	for _, e := range evs {
		if len(notices([]stream.Event{e})) == 0 {
			out = append(out, e)
		}
	}
	return out
}

func turnChange(enc data.Encounter, current string) data.Change {
	enc.CurrentCombatantID, enc.TurnQueue = current, []string{"orc", "pc"}
	return data.Change{Type: data.EventTurn, After: enc, SessionID: enc.SessionID, OwnerID: enc.OwnerUserID, At: time.Now()}
}

func TestTheTurnReachingACombatantNotifiesOnlyItsControllersWithoutStateOrRevision(t *testing.T) {
	n := newNoticeTable(t)
	gm := subscribe(n.hub, n.viewer(t, "user-ok"), encE1, 16)
	player := subscribe(n.hub, n.viewer(t, "user-two"), encE1, 16)
	observer := subscribe(n.hub, n.viewer(t, "user-three"), encE1, 16)

	n.hub.Emit(context.Background(), turnChange(n.encounter(t), "pc"))

	got := notices(drain(player))
	if len(got) != 1 || got[0].Type != data.NoticeTurnYours || got[0].Data["combatant_id"] != "pc" {
		t.Fatalf("player notices = %+v, want one turn.yours for pc", got)
	}
	if got[0].Revision != 0 || got[0].ViewTag != "" {
		t.Errorf("the notice carries revision %d / view_tag %q; neither belongs on an addressed event", got[0].Revision, got[0].ViewTag)
	}
	if g, o := notices(drain(gm)), notices(drain(observer)); len(g) != 0 || len(o) != 0 {
		t.Errorf("a non-controller received notices: gm %v observer %v", g, o)
	}
}

func TestTheNextCombatantsControllerHearsTurnNext(t *testing.T) {
	n := newNoticeTable(t)
	player := subscribe(n.hub, n.viewer(t, "user-two"), encE1, 16)
	n.hub.Emit(context.Background(), turnChange(n.encounter(t), "orc")) // pc is next in the queue
	got := notices(drain(player))
	if len(got) != 1 || got[0].Type != data.NoticeTurnNext {
		t.Fatalf("player notices = %+v, want one turn.next", got)
	}
}

type mutes map[string][]string

func (m mutes) Muted(_ context.Context, user, _ string) ([]string, error) { return m[user], nil }

func TestAMutedTypeIsNotDeliveredButStateEventsStillAre(t *testing.T) {
	n := newNoticeTable(t)
	n.hub.SetPreferences(mutes{"u-2": {data.NoticeTurnNext}})
	player := subscribe(n.hub, n.viewer(t, "user-two"), encE1, 16)
	n.hub.Emit(context.Background(), turnChange(n.encounter(t), "orc"))
	evs := drain(player)
	if got := notices(evs); len(got) != 0 {
		t.Errorf("muted turn.next delivered: %+v", got)
	}
	if len(evs) == 0 {
		t.Error("the state event was dropped along with the muted notice")
	}
}

func TestChangeRequestNoticesGoToManagersThenTheSubmitterWithoutTheProposedValues(t *testing.T) {
	n := newNoticeTable(t)
	gm := subscribe(n.hub, n.viewer(t, "user-ok"), encE1, 16)
	player := subscribe(n.hub, n.viewer(t, "user-two"), encE1, 16)
	code, req := submit(t, n.r, "user-two", n.sid, "pc", `{"health":{"hp":4}}`)
	if code != http.StatusCreated {
		t.Fatalf("submit = %d", code)
	}
	if got := notices(drain(gm)); len(got) != 1 || got[0].Type != data.NoticeChangeSubmitted || got[0].Data["change_request_id"] != req["id"] {
		t.Fatalf("GM notices = %+v, want one change_request.submitted", got)
	}
	if got := notices(drain(player)); len(got) != 0 {
		t.Errorf("the submitter was told of its own submission: %+v", got)
	}
	w := callAs(n.r, "user-ok", "POST", encPath(n.sid, encE1)+"/change-requests/"+req["id"].(string)+"/approve", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	got := notices(drain(player))
	if len(got) != 1 || got[0].Type != data.NoticeChangeApproved {
		t.Fatalf("player notices = %+v, want one change_request.approved", got)
	}
	if _, leaked := got[0].Data["health"]; leaked {
		t.Errorf("the decision carries the proposed values: %v", got[0].Data)
	}
}

func TestPreferencesAreOwnedByTheCallerAndRejectUnknownTypes(t *testing.T) {
	n := newNoticeTable(t)
	path := "/sessions/" + n.sid + "/notification-preferences"
	if w := callAs(n.r, "user-two", "PUT", path, `{"muted":["turn.next","turn.next"]}`); w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	if got := decodeBody(t, callAs(n.r, "user-two", "GET", path, ""))["muted"].([]any); len(got) != 1 || got[0] != "turn.next" {
		t.Errorf("player's mutes = %v, want [turn.next]", got)
	}
	if got := decodeBody(t, callAs(n.r, "user-three", "GET", path, ""))["muted"].([]any); len(got) != 0 {
		t.Errorf("another member sees %v, want their own empty list", got)
	}
	if w := callAs(n.r, "user-two", "PUT", path, `{"muted":["bogus"]}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown type = %d, want 422", w.Code)
	}
	if w := callAs(n.r, "user-two", "PUT", "/sessions/5f00000000000000000000d0/notification-preferences", `{"muted":[]}`); w.Code == http.StatusOK {
		t.Errorf("a non-member stored preferences for a session it is not in")
	}
}
