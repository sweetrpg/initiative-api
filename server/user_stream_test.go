package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sweetrpg/initiative-api/data"
	"go.mongodb.org/mongo-driver/bson"
)

func userStreamEncounter(t *testing.T, st streamTable) data.Encounter {
	t.Helper()
	var enc data.Encounter
	if err := st.db.Fake(data.CollectionEncounters).FindOne(context.Background(), bson.D{{Key: "_id", Value: encE1}}, &enc); err != nil {
		t.Fatal(err)
	}
	return enc
}

func (st streamTable) openUserLevel(t *testing.T, token string) (*websocket.Conn, map[string]any) {
	t.Helper()
	tk, code := st.ticket(t, token, `{"scope":"user"}`)
	if code != http.StatusCreated {
		t.Fatalf("user-level ticket = %d", code)
	}
	c, _, err := st.dial(t, "ticket="+tk, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	hello, _ := frame(t, c)
	return c, hello
}

func notice(session, user, typ string) data.Notice {
	return data.Notice{Type: typ, SessionID: session, EncounterID: encE1, Users: []string{user}, At: time.Now(), Data: map[string]any{"combatant_id": "pc"}}
}

func TestTheUserLevelStreamHearsOnlyItsUsersNoticesAcrossSessionsAndNoState(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	c, hello := st.openUserLevel(t, "user-two")
	if hello["type"] != "hello" || hello["scope"] != "user" || hello["session_id"] != "" {
		t.Fatalf("hello = %v, want a user-level hello with no session", hello)
	}
	ctx := context.Background()
	st.hub.Notify(ctx, notice(st.sid, "u-3", data.NoticeTurnYours))                                                              // someone else's
	st.hub.Notify(ctx, notice("5f00000000000000000000d0", "u-2", data.NoticeTurnYours))                                          // a session u-2 is not in
	st.hub.Emit(ctx, data.Change{Type: data.EventUpdated, After: userStreamEncounter(t, st), SessionID: st.sid, At: time.Now()}) // state, not addressed
	st.hub.Notify(ctx, notice(st.sid, "u-2", data.NoticeTurnNext))
	got, raw := frame(t, c)
	if got["type"] != data.NoticeTurnNext {
		t.Fatalf("first frame = %s, want only the notice addressed to u-2 in a session they belong to", raw)
	}
}

func TestAUserLevelTicketTakesNoScopeAndCredentialsCannotOpenOne(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	for name, body := range map[string]string{
		"with a session":    `{"scope":"user","session_id":"` + st.sid + `"}`,
		"with an encounter": `{"scope":"user","encounter_id":"` + encE1 + `"}`,
		"unknown scope":     `{"scope":"everything"}`,
	} {
		if _, code := st.ticket(t, "user-ok", body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
	_, secret := issue(t, st.r, "user-ok", `["read"]`)
	if got := st.refused(t, "scope=user", http.Header{"Authorization": {"Bearer " + secret}}); got != http.StatusForbidden {
		t.Errorf("a credential on the user-level stream = %d, want 403", got)
	}
}

func TestAUserRemovedAfterOpeningTheUserLevelStreamHearsNothingFurther(t *testing.T) {
	st := newStreamTable(t, StreamOptions{}, 0)
	c, _ := st.openUserLevel(t, "user-two")
	ctx := context.Background()

	st.hub.Notify(ctx, notice(st.sid, "u-2", data.NoticeTurnYours))
	if got, raw := frame(t, c); got["type"] != data.NoticeTurnYours {
		t.Fatalf("before the removal the member got %s, want the notice", raw)
	}
	if w := callAs(st.r, "user-ok", "DELETE", "/sessions/"+st.sid+"/members/u-2", ""); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	st.hub.Notify(ctx, notice(st.sid, "u-2", data.NoticeTurnNext))
	st.hub.Flush()

	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, b, err := c.Read(rctx); err == nil {
		t.Errorf("a removed member's user-level stream received %s", b)
	}
}
