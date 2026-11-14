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

func (f fakeMembers) SessionMembers(_ context.Context, id string) ([]data.Member, error) {
	var out []data.Member
	for _, m := range f {
		if m.SessionID == id {
			out = append(out, m)
		}
	}
	return out, nil
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
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s1", "u2", "player")}, ft, nil, nil}.Run(context.Background())
	if err != nil || res.Written != 2 || len(ft.writes) != 2 {
		t.Fatalf("res=%+v err=%v writes=%v", res, err, ft.writes)
	}
}

func TestExtraTupleIsDeleted(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm"), tup("s1", "gone", "player")}}}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft, nil, nil}.Run(context.Background())
	if err != nil || res.Deleted != 1 || ft.deletes[0] != tup("s1", "gone", "player") || res.Written != 0 {
		t.Fatalf("res=%+v err=%v deletes=%v", res, err, ft.deletes)
	}
}

func TestRoleChangeReplacesTheOldTuple(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u2", "player")}}}
	_, err := Reconciler{fakeMembers{member("s1", "u2", "co_gm")}, ft, nil, nil}.Run(context.Background())
	if err != nil || len(ft.writes) != 1 || len(ft.deletes) != 1 || ft.calls != 1 {
		t.Fatalf("err=%v writes=%v deletes=%v calls=%d; want one batch with both", err, ft.writes, ft.deletes, ft.calls)
	}
}

func TestEqualStateWritesNothing(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm")}}}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft, nil, nil}.Run(context.Background())
	if err != nil || ft.calls != 0 || res.Written+res.Deleted != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, ft.calls)
	}
}

func TestDeletedMemberAndDeletedSessionLoseTheirTuples(t *testing.T) {
	now := time.Now()
	gone := member("s2", "u9", "player")
	gone.DeletedAt = &now
	ft := &fakeTuples{have: map[string][]Tuple{"session:s2": {tup("s2", "u9", "player")}}}
	res, err := Reconciler{fakeMembers{gone}, ft, nil, nil}.Run(context.Background())
	if err != nil || res.Deleted != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForeignTuplesAreLeftAlone(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm"), {User: "session:x", Relation: "parent", Object: "session:s1"}}}}
	res, _ := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft, nil, nil}.Run(context.Background())
	if res.Deleted != 0 {
		t.Fatalf("deleted a tuple this job does not own: %+v", ft.deletes)
	}
}

func TestRejectedTokenStopsThePass(t *testing.T) {
	ft := &fakeTuples{err: ErrTokenRejected}
	_, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s2", "u1", "gm")}, ft, nil, nil}.Run(context.Background())
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
}

func TestOneFailureDoesNotSkipTheRest(t *testing.T) {
	ft := &fakeTuples{failObject: "session:s1"}
	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm"), member("s2", "u1", "gm")}, ft, nil, nil}.Run(context.Background())
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
	res, err := Reconciler{ms, ft, nil, nil}.Run(context.Background())
	if err != nil || res.Written != 250 || ft.calls != 3 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, ft.calls)
	}
}

// orderMembers records when a session's members are read relative to its tuples.
type orderMembers struct {
	fakeMembers
	log *[]string
}

func (o orderMembers) SessionMembers(ctx context.Context, id string) ([]data.Member, error) {
	*o.log = append(*o.log, "members:"+id)
	return o.fakeMembers.SessionMembers(ctx, id)
}

type orderTuples struct {
	*fakeTuples
	log *[]string
}

func (o orderTuples) Read(ctx context.Context, object string) ([]Tuple, error) {
	*o.log = append(*o.log, "tuples:"+object)
	return o.fakeTuples.Read(ctx, object)
}

func TestTuplesAreReadBeforeTheSessionsMembers(t *testing.T) {
	var log []string
	r := Reconciler{orderMembers{fakeMembers{member("s1", "u1", "gm")}, &log}, orderTuples{&fakeTuples{}, &log}, nil, nil}

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(log) != 2 || log[0] != "tuples:session:s1" || log[1] != "members:s1" {
		t.Fatalf("read order = %v, want the tuples first, then that session's members", log)
	}
}

func TestAPlantedCoGMTupleWithNoMemberIsDeleted(t *testing.T) {
	ft := &fakeTuples{have: map[string][]Tuple{"session:s1": {tup("s1", "u1", "gm"), tup("s1", "planted", "co_gm"), tup("s1", "planted2", "player")}}}

	res, err := Reconciler{fakeMembers{member("s1", "u1", "gm")}, ft, nil, nil}.Run(context.Background())

	if err != nil || res.Deleted != 2 || len(ft.writes) != 0 {
		t.Fatalf("res=%+v err=%v deletes=%v; want both planted tuples deleted", res, err, ft.deletes)
	}
}

type fakeMints struct {
	pages   [][]Mint // pages[i] is returned for cursor "p<i>"
	closed  []string
	listErr error
	closeE  error
	gotAge  time.Duration
	cursors []string
}

func (f *fakeMints) ListMints(_ context.Context, age time.Duration, after string) ([]Mint, string, error) {
	f.gotAge = age
	f.cursors = append(f.cursors, after)
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	i := 0
	if after != "" {
		i = int(after[1] - '0')
	}
	next := ""
	if i+1 < len(f.pages) {
		next = "p" + string(rune('0'+i+1))
	}
	return f.pages[i], next, nil
}

func (f *fakeMints) CloseMint(_ context.Context, id string) error {
	if f.closeE != nil {
		return f.closeE
	}
	f.closed = append(f.closed, id)
	return nil
}

type fakeSessionLookup struct {
	found map[string]bool // id -> deleted
	err   error
	calls int
}

func (f *fakeSessionLookup) LookupSessions(_ context.Context, ids []string) (map[string]bool, error) {
	f.calls++
	return f.found, f.err
}

func gc(mints *fakeMints, lookup *fakeSessionLookup) (Result, error) {
	return Reconciler{fakeMembers{}, &fakeTuples{}, mints, lookup}.Run(context.Background())
}

func TestGarbageCollectionClosesMissingAndDeletedButNeverLiveSessions(t *testing.T) {
	mints := &fakeMints{pages: [][]Mint{{{ID: "missing"}, {ID: "deleted"}, {ID: "live"}}}}
	lookup := &fakeSessionLookup{found: map[string]bool{"deleted": true, "live": false}}

	res, err := gc(mints, lookup)

	if err != nil || res.MintsClosed != 2 || len(mints.closed) != 2 || mints.closed[0] != "missing" || mints.closed[1] != "deleted" {
		t.Fatalf("res=%+v err=%v closed=%v; want missing and deleted closed, live left", res, err, mints.closed)
	}
	if mints.gotAge != 15*time.Minute {
		t.Errorf("min age = %v, want 15m so a session still being created is skipped", mints.gotAge)
	}
}

func TestGarbageCollectionSkipsABatchWhenTheLookupFails(t *testing.T) {
	mints := &fakeMints{pages: [][]Mint{{{ID: "a"}, {ID: "b"}}}}

	res, err := gc(mints, &fakeSessionLookup{err: errors.New("mongo timeout")})

	if err != nil || len(mints.closed) != 0 || res.MintsSkipped != 2 {
		t.Fatalf("res=%+v err=%v closed=%v; want nothing closed and 2 skipped", res, err, mints.closed)
	}
}

func TestGarbageCollectionPagesToTheEnd(t *testing.T) {
	mints := &fakeMints{pages: [][]Mint{{{ID: "live1"}}, {{ID: "live2"}}, {{ID: "orphan"}}}}
	lookup := &fakeSessionLookup{found: map[string]bool{"live1": false, "live2": false}}

	res, err := gc(mints, lookup)

	if err != nil || len(mints.closed) != 1 || mints.closed[0] != "orphan" || res.MintsClosed != 1 {
		t.Fatalf("res=%+v err=%v closed=%v; want the orphan on the last page found", res, err, mints.closed)
	}
	if got := mints.cursors; len(got) != 3 || got[0] != "" || got[1] != "p1" || got[2] != "p2" {
		t.Errorf("cursors = %v, want the previous page's next each time", got)
	}
	if lookup.calls != 3 {
		t.Errorf("lookups = %d, want one per page", lookup.calls)
	}
}

func TestGarbageCollectionStopsOnARejectedToken(t *testing.T) {
	_, err := gc(&fakeMints{listErr: ErrTokenRejected}, &fakeSessionLookup{})
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("list with a rejected token = %v, want ErrTokenRejected", err)
	}
	_, err = gc(&fakeMints{pages: [][]Mint{{{ID: "x"}}}, closeE: ErrTokenRejected}, &fakeSessionLookup{})
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("close with a rejected token = %v, want ErrTokenRejected", err)
	}
}

func TestAFailedListOrCloseIsCountedNotFatal(t *testing.T) {
	res, err := gc(&fakeMints{listErr: errors.New("503")}, &fakeSessionLookup{})
	if err == nil || res.Failed != 1 {
		t.Errorf("list failure: res=%+v err=%v, want a counted failure", res, err)
	}
	res, err = gc(&fakeMints{pages: [][]Mint{{{ID: "x"}}}, closeE: errors.New("503")}, &fakeSessionLookup{})
	if err == nil || res.Failed != 1 {
		t.Errorf("close failure: res=%+v err=%v, want a counted failure", res, err)
	}
}
