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
