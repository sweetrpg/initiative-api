package data

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

type leaseClock struct{ ns atomic.Int64 }

func (c *leaseClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *leaseClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func newLeases(t *testing.T) (*StreamLeases, *fake.Collection, *leaseClock) {
	t.Helper()
	db := fake.NewDB()
	r := NewStreamLeases(db)
	clk := &leaseClock{}
	clk.ns.Store(time.Now().UnixNano())
	r.SetClock(clk.now)
	return r, db.Fake(CollectionStreamLeases), clk
}

func lease(id, user, client string) StreamLease {
	return StreamLease{ID: id, UserID: user, ClientID: client, Pod: "pod-a"}
}

func TestConcurrentAcquiresNeverLeaveABucketOverItsCap(t *testing.T) {
	const max = 3
	r, col, _ := newLeases(t)
	ctx := context.Background()
	for i := range max - 1 {
		if err := r.Acquire(ctx, lease("seed"+strconv.Itoa(i), "u-1", ""), time.Minute, max); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var granted atomic.Int32
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Acquire(ctx, lease("c"+strconv.Itoa(i), "u-1", ""), time.Minute, max); err == nil {
				granted.Add(1)
			} else if !errors.Is(err, ErrStreamLimit) {
				t.Errorf("acquire: %v", err)
			}
		}()
	}
	wg.Wait()
	n, _ := col.CountDocuments(ctx, bson.D{{Key: "bucket", Value: "user:u-1"}})
	if n > max {
		t.Errorf("%d seats held, over the cap of %d", n, max)
	}
	if n < max-1 {
		t.Errorf("%d seats held, fewer than the %d that were already granted", n, max-1)
	}
	if int64(granted.Load())+int64(max-1) != n {
		t.Errorf("granted %d new seats but %d rows remain beyond the seeds", granted.Load(), n-int64(max-1))
	}
}

func TestARenewalExtendsTheSeatAndAnUnrenewedOneLapses(t *testing.T) {
	r, _, clk := newLeases(t)
	ctx := context.Background()
	l := lease("s1", "u-1", "")
	if err := r.Acquire(ctx, l, time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	clk.advance(50 * time.Second)
	if err := r.Renew(ctx, l, time.Minute); err != nil {
		t.Fatal(err)
	}
	clk.advance(50 * time.Second) // 100s after the acquire, 50s after the renewal
	if err := r.Acquire(ctx, lease("s2", "u-1", ""), time.Minute, 1); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a renewed seat should still count: %v", err)
	}
	clk.advance(11 * time.Second) // past the renewed end
	if err := r.Acquire(ctx, lease("s3", "u-1", ""), time.Minute, 1); err != nil {
		t.Errorf("a lapsed seat should free the bucket: %v", err)
	}
}

func TestRenewPutsBackARowTheTTLIndexRemoved(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	l := lease("s1", "u-1", "")
	if err := r.Acquire(ctx, l, time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	_ = col.DeleteOne(ctx, bson.D{{Key: "_id", Value: "s1"}})
	if err := r.Renew(ctx, l, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, _ := col.CountDocuments(ctx, bson.D{{Key: "_id", Value: "s1"}}); n != 1 {
		t.Errorf("rows = %d, want the seat re-inserted", n)
	}
}

func TestAReconnectTakesOverEveryDroppedSeatOfItsClientOnlyOnceItHasOne(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} { // two live streams sharing a client id, both dropped
		l := lease(id, "u-1", "web-1")
		if err := r.Acquire(ctx, l, time.Minute, 5); err != nil {
			t.Fatal(err)
		}
		if err := r.Release(ctx, l, time.Minute, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Acquire(ctx, lease("c", "u-1", "web-1"), time.Minute, 1); err != nil {
		t.Fatalf("reconnect with the cap at 1 = %v, want it to take over the dropped seats", err)
	}
	if n, _ := col.CountDocuments(ctx, bson.D{{Key: "bucket", Value: "user:u-1"}}); n != 1 {
		t.Errorf("%d rows, want only the new seat", n)
	}
}

func TestARefusedReconnectKeepsItsDroppedSeat(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	old := lease("old", "u-1", "web-1")
	_ = r.Acquire(ctx, old, time.Minute, 2)
	_ = r.Release(ctx, old, time.Minute, true)
	_ = r.Acquire(ctx, lease("other", "u-1", "web-2"), time.Minute, 2)
	if err := r.Acquire(ctx, lease("new", "u-1", "web-3"), time.Minute, 2); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("a third client with two seats held = %v, want the limit", err)
	}
	if n, _ := col.CountDocuments(ctx, bson.D{{Key: "_id", Value: "old"}}); n != 1 {
		t.Error("a refused connect removed another client's dropped seat")
	}
}

func TestGraceKeepsTheSeatThenFreesIt(t *testing.T) {
	r, _, clk := newLeases(t)
	ctx := context.Background()
	l := lease("s1", "u-1", "web-1")
	_ = r.Acquire(ctx, l, time.Minute, 1)
	_ = r.Release(ctx, l, 30*time.Second, true)
	if err := r.Acquire(ctx, lease("s2", "u-1", "web-2"), time.Minute, 1); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("during the grace = %v, want the limit", err)
	}
	clk.advance(31 * time.Second)
	if err := r.Acquire(ctx, lease("s3", "u-1", "web-2"), time.Minute, 1); err != nil {
		t.Errorf("after the grace = %v, want a free seat", err)
	}
}

func TestAServerEndedSeatIsFreedAtOnce(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	l := lease("s1", "u-1", "web-1")
	_ = r.Acquire(ctx, l, time.Minute, 1)
	_ = r.Release(ctx, l, time.Minute, false)
	if n, _ := col.CountDocuments(ctx, bson.D{}); n != 0 {
		t.Errorf("%d rows left after a server-ended stream, want none", n)
	}
}

type failingInsert struct{ store.Collection }

func (failingInsert) InsertOne(context.Context, any) error { return errors.New("mongo down") }

type failingDB struct{ store.Database }

func (d failingDB) Collection(name string) store.Collection {
	return failingInsert{d.Database.Collection(name)}
}

func TestAStoreFailureIsUnavailableNotALimit(t *testing.T) {
	r := NewStreamLeases(failingDB{fake.NewDB()})
	if err := r.Acquire(context.Background(), lease("s1", "u-1", ""), time.Minute, 3); !errors.Is(err, ErrStreamUnavailable) {
		t.Errorf("err = %v, want ErrStreamUnavailable", err)
	}
}

func TestAnOverLimitSeatIsGivenBackEvenWhenTheRequestWasCancelled(t *testing.T) {
	r, col, _ := newLeases(t)
	_ = r.Acquire(context.Background(), lease("s1", "u-1", ""), time.Minute, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.Acquire(ctx, lease("s2", "u-1", ""), time.Minute, 1)
	if n, _ := col.CountDocuments(context.Background(), bson.D{{Key: "_id", Value: "s2"}}); n != 0 {
		t.Error("a refused seat stayed behind after the client went away")
	}
}

func TestASessionBucketCountsConnectionsOfEveryPodAndUser(t *testing.T) {
	r, _, _ := newLeases(t)
	ctx := context.Background()
	seat := func(id, user, pod string) StreamLease {
		return StreamLease{ID: id, Bucket: "session:5f00000000000000000000a1", UserID: user, SessionID: "5f00000000000000000000a1", Pod: pod}
	}
	if err := r.Acquire(ctx, seat("s1", "u-1", "pod-a"), time.Minute, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.Acquire(ctx, seat("s2", "u-2", "pod-b"), time.Minute, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.Acquire(ctx, seat("s3", "u-3", "pod-c"), time.Minute, 2); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("third connection from a third pod = %v, want ErrStreamLimit: the count spans pods and users", err)
	}
}

func sessionSeats(t *testing.T, col *fake.Collection, extra ...bson.E) int64 {
	t.Helper()
	n, err := col.CountDocuments(context.Background(), append(bson.D{{Key: "bucket", Value: "session:s1"}}, extra...))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sessionLease(id, user, client string) StreamLease {
	l := lease(id, user, client)
	l.Bucket = "session:s1"
	return l
}

func TestTheSameMemberTakesTheirGraceSeatBackWithTheirClientID(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	a := sessionLease("a1", "u-1", "web")
	if err := r.Acquire(ctx, a, time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(ctx, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}

	if err := r.Acquire(ctx, sessionLease("a2", "u-1", "web"), time.Minute, 1); err != nil {
		t.Fatalf("the reconnect of the member that dropped: %v", err)
	}
	if n := sessionSeats(t, col); n != 1 {
		t.Errorf("%d seats after the reconnect, want the dropped one replaced (1)", n)
	}
}

func counted(user, id, client string) StreamLease {
	l := sessionLease(id, user, client)
	l.Counted = true
	return l
}

func TestAMembersDevicesShareOneMemberSeat(t *testing.T) {
	r, _, _ := newLeases(t)
	ctx := context.Background()
	for i, u := range []string{"u-1", "u-2"} {
		if err := r.AcquireSession(ctx, counted(u, "first"+strconv.Itoa(i), ""), time.Minute, 2, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.AcquireSession(ctx, counted("u-1", "second", ""), time.Minute, 2, 2); err != nil {
		t.Errorf("a second device of a member in a full session = %v, want a shared seat", err)
	}
	if err := r.AcquireSession(ctx, counted("u-3", "other", ""), time.Minute, 2, 2); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a third member = %v, want ErrStreamLimit", err)
	}
}

func TestAMemberOverTheDeviceLimitGetsTheTypedError(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	for _, id := range []string{"d1", "d2"} {
		if err := r.AcquireSession(ctx, sessionLease(id, "u-1", ""), time.Minute, 0, 2); err != nil { // the GM: uncounted
			t.Fatal(err)
		}
	}
	err := r.AcquireSession(ctx, sessionLease("d3", "u-1", ""), time.Minute, 0, 2)
	var dl *DeviceLimitError
	if !errors.As(err, &dl) || dl.Limit != 2 {
		t.Fatalf("a third device = %v, want *DeviceLimitError with limit 2", err)
	}
	if n := sessionSeats(t, col); n != 2 {
		t.Errorf("%d seats after the refusal, want the refused one given back (2)", n)
	}
}

func TestADroppedClientIsNotADeviceButStillHoldsItsMemberSeat(t *testing.T) {
	r, _, _ := newLeases(t)
	ctx := context.Background()
	a := counted("u-1", "a1", "web")
	if err := r.AcquireSession(ctx, a, time.Minute, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(ctx, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}

	if err := r.AcquireSession(ctx, counted("u-1", "a2", "phone"), time.Minute, 1, 1); err != nil {
		t.Errorf("a new device while the old one is in grace = %v, want it allowed (a dropped client is not a device)", err)
	}
	if err := r.AcquireSession(ctx, counted("u-2", "b1", "web"), time.Minute, 1, 1); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("another member = %v, want ErrStreamLimit", err)
	}
}

func TestAnotherMembersClientIDCannotTakeOverAGraceSeatInASession(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	a := counted("u-1", "a1", "web")
	if err := r.AcquireSession(ctx, a, time.Minute, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(ctx, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}

	if err := r.AcquireSession(ctx, counted("u-2", "b1", "web"), time.Minute, 3, 2); err != nil {
		t.Fatal(err)
	}
	if n := sessionSeats(t, col, bson.E{Key: "_id", Value: "a1"}, bson.E{Key: "dropped", Value: true}); n != 1 {
		t.Errorf("u-1's grace seat survives u-2 using the same client id: %d rows, want 1", n)
	}
	if err := r.AcquireSession(ctx, counted("u-3", "c1", "tab"), time.Minute, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.AcquireSession(ctx, counted("u-4", "d1", "tab"), time.Minute, 3, 2); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a fourth member with u-1's grace seat still held = %v, want ErrStreamLimit", err)
	}
}

func TestConcurrentFirstConnectsOfDifferentMembersNeverLeaveTheSessionOverItsLimit(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	if err := r.AcquireSession(ctx, counted("u-0", "seed", ""), time.Minute, 3, 2); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.AcquireSession(ctx, counted("u-"+strconv.Itoa(i+1), "c"+strconv.Itoa(i), ""), time.Minute, 3, 2); err != nil && !errors.Is(err, ErrStreamLimit) {
				t.Errorf("acquire: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := sessionSeats(t, col); n > 3 {
		t.Errorf("%d member seats held, over the limit of 3", n)
	}
}

func TestAnotherMembersDevicesCountAsOneMemberForANewMember(t *testing.T) {
	r, _, _ := newLeases(t)
	ctx := context.Background()
	for _, c := range []struct{ user, id string }{{"u-1", "a1"}, {"u-1", "a2"}, {"u-2", "b1"}} {
		if err := r.AcquireSession(ctx, counted(c.user, c.id, ""), time.Minute, 3, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.AcquireSession(ctx, counted("u-3", "c1", ""), time.Minute, 3, 2); err != nil {
		t.Errorf("the third member with u-1 on two devices = %v, want it allowed: 3 members, 4 connections", err)
	}
}

func TestAFirstConnectIsNotShieldedByItsOwnMembersPendingSeat(t *testing.T) {
	r, col, _ := newLeases(t)
	ctx := context.Background()
	for i, u := range []string{"u-1", "u-2"} {
		if err := r.AcquireSession(ctx, counted(u, "seat"+strconv.Itoa(i), ""), time.Minute, 2, 2); err != nil {
			t.Fatal(err)
		}
	}
	racing := counted("u-3", "racing", "")
	racing.Pending, racing.Until = true, time.Now().Add(time.Minute) // u-3's other first connect, still in flight
	if err := col.InsertOne(ctx, racing); err != nil {
		t.Fatal(err)
	}

	if err := r.AcquireSession(ctx, counted("u-3", "mine", ""), time.Minute, 2, 2); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a first connect beside the same member's pending seat in a full session = %v, want ErrStreamLimit", err)
	}
}

func TestTwoFirstConnectsOfOneNewMemberRacingInAFullSessionAreBothRefused(t *testing.T) {
	for round := range 200 {
		r, col, _ := newLeases(t)
		ctx := context.Background()
		for i, u := range []string{"u-1", "u-2"} {
			if err := r.AcquireSession(ctx, counted(u, "seat"+strconv.Itoa(i), ""), time.Minute, 2, 2); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		var granted atomic.Int32
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := r.AcquireSession(ctx, counted("u-3", "r"+strconv.Itoa(i), ""), time.Minute, 2, 2); err == nil {
					granted.Add(1)
				} else if !errors.Is(err, ErrStreamLimit) {
					t.Errorf("acquire: %v", err)
				}
			}()
		}
		wg.Wait()
		if granted.Load() != 0 {
			t.Fatalf("round %d: %d of u-3's two first connects were accepted into a full session", round, granted.Load())
		}
		if n, _ := col.CountDocuments(ctx, bson.D{{Key: "user_id", Value: "u-3"}}); n != 0 {
			t.Fatalf("round %d: %d seats left behind by the refused connects", round, n)
		}
	}
}

func TestAMemberWithASeatAddsADeviceAfterTheLimitFellBelowTheMembersConnected(t *testing.T) {
	r, _, _ := newLeases(t)
	ctx := context.Background()
	for i, u := range []string{"u-1", "u-2"} {
		if err := r.AcquireSession(ctx, counted(u, "seat"+strconv.Itoa(i), ""), time.Minute, 2, 2); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.AcquireSession(ctx, counted("u-1", "second", ""), time.Minute, 1, 2); err != nil {
		t.Errorf("a second device of a seated member with the limit fallen to 1 = %v, want it shared", err)
	}
	if err := r.AcquireSession(ctx, counted("u-3", "new", ""), time.Minute, 1, 2); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a new member with the limit fallen to 1 = %v, want ErrStreamLimit", err)
	}
}
