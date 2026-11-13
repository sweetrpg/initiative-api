package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

type fakeMembers []data.Member

func (f fakeMembers) ScanMembers(_ context.Context, q data.Query) (data.Page[data.Member], error) {
	return data.Page[data.Member]{Items: f}, nil
}

type fakeTuples struct {
	have            map[string][]Tuple
	writes, deletes []Tuple
	calls           int
	failObject      string
	err             error
}

func (f *fakeTuples) Read(_ context.Context, object string) ([]Tuple, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.have[object], nil
}

func (f *fakeTuples) Write(_ context.Context, w, d []Tuple) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if len(w)+len(d) > maxBatch {
		return errors.New("batch too large")
	}
	if f.failObject != "" && len(w) > 0 && w[0].Object == f.failObject {
		return errors.New("boom")
	}
	f.writes, f.deletes = append(f.writes, w...), append(f.deletes, d...)
	return nil
}

func member(session, user, role string) data.Member {
	return data.Member{SessionID: session, UserID: user, Role: role}
}

func tup(session, user, rel string) Tuple {
	return Tuple{User: "user:" + user, Relation: rel, Object: "session:" + session}
}

func TestMissingTupleIsWritten(t *testing.T) {
	ft := &fakeTuples{}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s1", "u2", "player")}, ft}.Run(context.Background())
	if err != nil || res.Written != 2 || len(ft.writes) != 2 {
		t.Fatalf("res=%+v err=%v writes=%v", res, err, ft.writes)
	}
}

func TestExtraTupleIsDeleted(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm"), tup("s1", "gone", "player")}}}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft}.Run(context.Background())
	if err != nil || res.Deleted != 1 || ft.deletes[0] != tup("s1", "gone", "player") || res.Written != 0 {
		t.Fatalf("res=%+v err=%v deletes=%v", res, err, ft.deletes)
	}
}

func TestRoleChangeReplacesTheOldTuple(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u2", "player")}}}
	_, err := Reconciler{fakeMembers{member("s1", "u2", "co_gm")}, ft}.Run(context.Background())
	if err != nil || len(ft.writes) != 1 || len(ft.deletes) != 1 || ft.calls != 1 {
		t.Fatalf("err=%v writes=%v deletes=%v calls=%d; want one batch with both", err, ft.writes, ft.deletes, ft.calls)
	}
}

func TestEqualStateWritesNothing(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm")}}}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft}.Run(context.Background())
	if err != nil || ft.calls != 0 || res.Written+res.Deleted != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, ft.calls)
	}
}

func TestDeletedMemberAndDeletedSessionLoseTheirTuples(t *testing.T) {
	now := time.Now()
	gone := member("s2", "u9", "player")
	gone.DeletedAt = &now
	ft := &fakeTuples{have: map[string][]Tuple{"session:s2": {tup("s2", "u9", "player")}}}
	res, err := Reconciler{fakeMembers{gone}, ft}.Run(context.Background())
	if err != nil || res.Deleted != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForeignTuplesAreLeftAlone(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm"), {User: "session:x", Relation: "parent", Object: "session:s1"}}}}
	res, _ := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft}.Run(context.Background())
	if res.Deleted != 0 {
		t.Fatalf("deleted a tuple this job does not own: %+v", ft.deletes)
	}
}

func TestRejectedTokenStopsThePass(t *testing.T) {
	ft := &fakeTuples{err: ErrTokenRejected}
	_, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s2", "u1", "gm")}, ft}.Run(context.Background())
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
}

func TestOneFailureDoesNotSkipTheRest(t *testing.T) {
	ft := &fakeTuples{failObject: "session:s1"}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s2", "u1", "gm")}, ft}.Run(context.Background())
	if err == nil || res.Failed != 1 || len(ft.writes) != 1 || ft.writes[0].Object != "session:s2" {
		t.Fatalf("res=%+v err=%v writes=%v", res, err, ft.writes)
	}
}

func TestLargeDiffsAreBatched(t *testing.T) {
	var ms fakeMembers
	for i := range 250 {
		ms = append(ms, member("s1", "u"+string(rune('A'+i/26))+string(rune('a'+i%26)), "player"))
	}
	ft := &fakeTuples{}
	res, err := Reconciler{ms, ft}.Run(context.Background())
	if err != nil || res.Written != 250 || ft.calls != 3 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, ft.calls)
	}
}
