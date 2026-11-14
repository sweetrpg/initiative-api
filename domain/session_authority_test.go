package domain

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// seat makes a session with a GM and the given extra members, writing through fa.
func seat(t *testing.T, fa *fakeAuth, members map[string]string) (*Sessions, string) {
	t.Helper()
	ctx := withAuth(fa)
	s := newSessions(t)
	sess, err := s.Create(ctx, "gm", "Friday")
	if err != nil {
		t.Fatal(err)
	}
	for user, role := range members {
		_, code, err := s.CreateInvite(ctx, "gm", sess.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		jr, err := s.Join(ctx, user, code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Approve(ctx, "gm", sess.ID, jr.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	fa.writes = nil
	return s, sess.ID
}

func lastWrite(t *testing.T, fa *fakeAuth) tupleCall {
	t.Helper()
	if len(fa.writes) == 0 {
		t.Fatal("no tuple write reached auth-api")
	}
	return fa.writes[len(fa.writes)-1]
}

func TestCreateUsesTheMintedIDAndFailsClosedWithoutAuthAPI(t *testing.T) {
	fa := &fakeAuth{}
	s := newSessions(t)

	sess, err := s.Create(withAuth(fa), "gm", "Friday")

	if err != nil || len(fa.minted) != 1 || sess.ID != fa.minted[0] {
		t.Fatalf("create = %+v, %v; minted %v; want the session at the minted id", sess, err, fa.minted)
	}
	for name, mintErr := range map[string]error{"down": relations.ErrUnavailable, "denied": relations.ErrDenied, "limited": relations.ErrLimited, "token": relations.ErrUnauthorized} {
		bad := &fakeAuth{mintErr: mintErr}
		if _, err := s.Create(withAuth(bad), "gm", "Saturday"); !errors.Is(err, mintErr) {
			t.Errorf("%s: create = %v, want %v", name, err, mintErr)
		}
	}
	if page, _ := s.List(sctx, "gm", data.Query{}); len(page.Items) != 1 {
		t.Errorf("a refused mint left %d sessions, want only the first", len(page.Items))
	}
}

func TestCreateWithNoAuthorityFailsClosed(t *testing.T) {
	s := newSessions(t)

	_, err := s.Create(WithChecker(t.Context(), stubChecker{limit: 10}), "gm", "Friday")

	if !errors.Is(err, relations.ErrUnavailable) {
		t.Fatalf("create = %v, want relations.ErrUnavailable", err)
	}
}

func TestCreateEndsTheMintWhenTheInsertFails(t *testing.T) {
	fa := &fakeAuth{mintID: "not-an-object-id"}
	s := newSessions(t)

	_, err := s.Create(withAuth(fa), "gm", "Friday")

	if !errors.Is(err, data.ErrInvalid) || len(fa.ended) != 1 || fa.ended[0] != "not-an-object-id" {
		t.Fatalf("create = %v, ended %v; want ErrInvalid and the mint ended", err, fa.ended)
	}
}

func TestApproveWritesTheMembersTupleAfterMongo(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, nil)
	ctx := withAuth(fa)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
	jr, _ := s.Join(ctx, "p1", code)

	m, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer)

	w := lastWrite(t, fa)
	if err != nil || m.Role != data.RolePlayer || w.Session != id || len(w.Writes) != 1 || w.Writes[0] != tupleOf("p1", data.RolePlayer) {
		t.Fatalf("approve = %+v, %v; write %+v; want the player tuple on the session", m, err, w)
	}
}

func TestFailedTupleWriteKeepsTheMemberAndTheRetryFinishesIt(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, nil)
	ctx := withAuth(fa)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
	jr, _ := s.Join(ctx, "p1", code)
	fa.failW = relations.ErrUnavailable

	_, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer)

	if !errors.Is(err, ErrSyncPending) {
		t.Fatalf("approve with auth-api down = %v, want ErrSyncPending", err)
	}
	if _, err := s.Get(sctx, "p1", id); err != nil {
		t.Fatalf("the saved member is denied by Mongo too: %v", err)
	}
	fa.failW = nil
	if _, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer); err != nil {
		t.Fatalf("retry = %v, want success", err)
	}
	if w := lastWrite(t, fa); w.Writes[0] != tupleOf("p1", data.RolePlayer) {
		t.Errorf("retry wrote %+v, want the player tuple", w)
	}
}

func TestRejectedTokenOnATupleWriteIsReturned(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, nil)
	ctx := withAuth(fa)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
	jr, _ := s.Join(ctx, "p1", code)
	fa.failW = relations.ErrUnauthorized

	_, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer)

	if !errors.Is(err, relations.ErrUnauthorized) || errors.Is(err, ErrSyncPending) {
		t.Fatalf("approve = %v, want the rejected token, not a pending sync", err)
	}
}

func TestSetRoleSendsOneWriteThatReplacesTheOldRole(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, map[string]string{"p1": data.RoleObserver})

	if _, err := s.SetRole(withAuth(fa), "gm", id, "p1", data.RolePlayer); err != nil {
		t.Fatal(err)
	}

	w := lastWrite(t, fa)
	if len(w.Writes) != 1 || w.Writes[0] != tupleOf("p1", data.RolePlayer) {
		t.Fatalf("writes = %+v, want the player tuple", w.Writes)
	}
	want := map[relations.Tuple]bool{tupleOf("p1", data.RoleObserver): true, tupleOf("p1", data.RoleCoGM): true}
	for _, d := range w.Deletes {
		delete(want, d)
	}
	if len(want) != 0 {
		t.Errorf("deletes = %+v, want every other non-gm relation for the user", w.Deletes)
	}
}

func TestACoGMManagesPlayersButNeverTouchesCoGMs(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, map[string]string{"co": data.RoleCoGM, "co2": data.RoleCoGM, "p1": data.RolePlayer})
	co := withAuth(fa)

	if _, err := s.SetRole(co, "co", id, "p1", data.RoleObserver); err != nil {
		t.Fatalf("co_gm changes a player to observer = %v, want success", err)
	}
	for _, d := range lastWrite(t, fa).Deletes {
		if d.Relation == data.RoleCoGM {
			t.Errorf("a co_gm's write deletes a co_gm tuple %+v; auth-api would refuse it", d)
		}
	}
	checks := map[string]error{
		"appoint a co_gm": func() error { _, err := s.SetRole(co, "co", id, "p1", data.RoleCoGM); return err }(),
		"demote a co_gm":  func() error { _, err := s.SetRole(co, "co", id, "co2", data.RolePlayer); return err }(),
		"remove a co_gm":  s.Remove(co, "co", id, "co2"),
		"block a co_gm":   s.Block(co, "co", id, "co2"),
		"approve as a co_gm": func() error {
			_, code, _ := s.CreateInvite(co, "co", id, 0)
			jr, _ := s.Join(co, "newbie", code)
			_, err := s.Approve(co, "co", id, jr.ID, data.RoleCoGM)
			return err
		}(),
	}
	for name, err := range checks {
		if !errors.Is(err, data.ErrForbidden) {
			t.Errorf("co_gm tries to %s = %v, want ErrForbidden", name, err)
		}
	}
	if _, err := s.SetRole(withAuth(fa), "gm", id, "p1", data.RoleCoGM); err != nil {
		t.Errorf("gm appoints a co_gm = %v, want success", err)
	}
}

func TestRevocationsDeleteTheMembersTuplesAndNeverFailTheRequest(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, map[string]string{"p1": data.RolePlayer, "p2": data.RolePlayer, "p3": data.RoleObserver})
	ctx := withAuth(fa)

	if err := s.Remove(ctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, fa); len(w.Writes) != 0 || !slices.Contains(w.Deletes, tupleOf("p1", data.RolePlayer)) {
		t.Fatalf("remove wrote %+v, want the player tuple deleted and nothing written", w)
	}
	if err := s.Block(ctx, "gm", id, "p2"); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, fa); !slices.Contains(w.Deletes, tupleOf("p2", data.RolePlayer)) {
		t.Errorf("block wrote %+v, want p2's player tuple deleted", w)
	}
	if err := s.Leave(ctx, "p3", id); err != nil {
		t.Fatal(err)
	}
	if w := lastWrite(t, fa); len(w.Deletes) != 1 || w.Deletes[0] != tupleOf("p3", data.RoleObserver) {
		t.Errorf("leave wrote %+v, want only the caller's own tuple deleted", w)
	}

	fa.failW = relations.ErrUnavailable
	s2, id2 := seat(t, &fakeAuth{}, map[string]string{"p1": data.RolePlayer})
	if err := s2.Remove(WithAuthority(sctx, fa, "tok"), "gm", id2, "p1"); err != nil {
		t.Errorf("remove with the tuple delete failing = %v, want success (the member is already gone in Mongo)", err)
	}
	if _, err := s2.Get(sctx, "p1", id2); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("removed member still reads the session: %v", err)
	}
}

func TestDeletingASessionEndsItAtAuthAPI(t *testing.T) {
	fa := &fakeAuth{endErr: relations.ErrUnavailable}
	s, id := seat(t, fa, nil)
	sess, _ := s.Get(sctx, "gm", id)

	if err := s.Delete(withAuth(fa), "gm", id, sess.Revision); err != nil {
		t.Fatalf("delete with the end call failing = %v, want success", err)
	}

	if len(fa.ended) != 1 || fa.ended[0] != id {
		t.Errorf("ended = %v, want the session id", fa.ended)
	}
}

func TestAPlantedTupleWithoutAMongoMemberGrantsNothing(t *testing.T) {
	// The gate in sctx grants every relation, as OpenFGA would for planted co_gm tuples.
	s, id := seat(t, &fakeAuth{}, map[string]string{"obs": data.RoleObserver})

	if _, err := s.Get(sctx, "planted", id); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("a user with a tuple but no member record reads the session: %v", err)
	}
	if _, _, err := s.CreateInvite(sctx, "obs", id, 0); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("an observer holding a planted co_gm tuple invites: %v", err)
	}
}

// failDropDB makes member updates (the soft deletes DropSession does) fail once armed.
type failDropDB struct {
	*fake.DB
	armed *bool
}

func (d failDropDB) Collection(name string) store.Collection {
	c := d.DB.Collection(name)
	if name == data.CollectionSessionMembers {
		return failUpdateCol{c, d.armed}
	}
	return c
}

type failUpdateCol struct {
	store.Collection
	armed *bool
}

func (c failUpdateCol) FindOneAndUpdate(ctx context.Context, f, u bson.D, out any) error {
	if *c.armed {
		return errors.New("members down")
	}
	return c.Collection.FindOneAndUpdate(ctx, f, u, out)
}

func TestDeleteEndsTheSessionEvenWhenDroppingTheMembersFails(t *testing.T) {
	fa := &fakeAuth{}
	armed := false
	s := NewSessions(failDropDB{fake.NewDB(), &armed})
	ctx := withAuth(fa)
	sess, err := s.Create(ctx, "gm", "Friday")
	if err != nil {
		t.Fatal(err)
	}
	armed = true

	err = s.Delete(ctx, "gm", sess.ID, sess.Revision)

	if err == nil {
		t.Fatal("delete succeeded though dropping the members failed")
	}
	if len(fa.ended) != 1 || fa.ended[0] != sess.ID {
		t.Fatalf("ended = %v, want the session ended before the failing drop (a retry 404s once the session is soft-deleted)", fa.ended)
	}
}

func TestTheGMsRemovalClearsEveryMemberRelationButAnyoneElsesOnlyTheExactOne(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, map[string]string{"co": data.RoleCoGM, "p1": data.RolePlayer, "p2": data.RolePlayer})
	ctx := withAuth(fa)

	if err := s.Remove(ctx, "gm", id, "p1"); err != nil {
		t.Fatal(err)
	}
	if got := lastWrite(t, fa).Deletes; len(got) != 3 {
		t.Errorf("the GM's removal deleted %+v, want all three member relations (a stale co_gm tuple must not outlive it)", got)
	}
	if err := s.Remove(ctx, "co", id, "p2"); err != nil {
		t.Fatal(err)
	}
	if got := lastWrite(t, fa).Deletes; len(got) != 1 || got[0] != tupleOf("p2", data.RolePlayer) {
		t.Errorf("a co_gm's removal deleted %+v, want only the player tuple", got)
	}
}

func TestAuthAPIRefusalsOnAGrantAreReturnedNotHiddenAsPending(t *testing.T) {
	for name, refusal := range map[string]error{"denied": relations.ErrDenied, "limited": relations.ErrLimited, "full": relations.ErrConflict} {
		fa := &fakeAuth{}
		s, id := seat(t, fa, nil)
		ctx := withAuth(fa)
		_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
		jr, _ := s.Join(ctx, "p1", code)
		fa.failW = refusal

		_, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer)

		if !errors.Is(err, refusal) || errors.Is(err, ErrSyncPending) {
			t.Errorf("%s: approve = %v, want the refusal itself", name, err)
		}
		if _, gerr := s.Get(sctx, "p1", id); gerr != nil {
			t.Errorf("%s: the member saved in Mongo is not readable: %v", name, gerr)
		}
	}
}

func TestAMalformedTupleRequestIsNotReportedAsAPendingSync(t *testing.T) {
	fa := &fakeAuth{}
	s, id := seat(t, fa, nil)
	ctx := withAuth(fa)
	_, code, _ := s.CreateInvite(ctx, "gm", id, 0)
	jr, _ := s.Join(ctx, "p1", code)
	fa.failW = relations.ErrRejected

	_, err := s.Approve(ctx, "gm", id, jr.ID, data.RolePlayer)

	if !errors.Is(err, relations.ErrRejected) || errors.Is(err, ErrSyncPending) {
		t.Fatalf("approve = %v, want the rejection, not a retryable pending sync", err)
	}
}
