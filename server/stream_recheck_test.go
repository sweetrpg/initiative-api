package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// streamDBWrap, when set by a test, wraps the database newStreamTable builds the router on.
var streamDBWrap func(*fake.DB) store.Database

// readFailDB fails every FindOne while fail is set, as a store that cannot answer does.
type readFailDB struct {
	*fake.DB
	fail *atomic.Bool
}

func (d readFailDB) Collection(name string) store.Collection {
	return readFailCollection{d.DB.Collection(name), d.fail}
}

type readFailCollection struct {
	store.Collection
	fail *atomic.Bool
}

func (c readFailCollection) FindOne(ctx context.Context, f bson.D, out any) error {
	if c.fail.Load() {
		return errors.New("mongo: server selection timeout")
	}
	return c.Collection.FindOne(ctx, f, out)
}

const testRecheck = 40 * time.Millisecond

// The re-check is the only thing that can notice: the evictor is off, as when an eviction is lost.
func TestARemovedMembersStreamClosesWithinTheRevocationBoundWhenTheEvictionIsLost(t *testing.T) {
	opts := StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}
	st := newStreamTable(t, opts, 0)
	st.svc.Sessions.SetEvictor(nil)
	c, _ := st.open(t, "user-two", "")
	start := time.Now()
	if w := callAs(st.r, "user-ok", "DELETE", "/sessions/"+st.sid+"/members/u-2", ""); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Fatalf("close code = %d, want %d", got, CloseMembershipChanged)
	}
	if took := time.Since(start); took > opts.RevocationBound() {
		t.Errorf("the stream outlived the removal by %s, bound %s", took, opts.RevocationBound())
	}
}

func TestADemotedMembersStreamClosesWithinTheBoundToo(t *testing.T) {
	opts := StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}
	st := newStreamTable(t, opts, 0)
	st.svc.Sessions.SetEvictor(nil)
	c, _ := st.open(t, "user-two", "")
	start := time.Now()
	if w := callAs(st.r, "user-ok", "PATCH", "/sessions/"+st.sid+"/members/u-2", `{"role":"observer"}`); w.Code >= 300 {
		t.Fatalf("demote = %d", w.Code)
	}
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Fatalf("close code = %d, want %d", got, CloseMembershipChanged)
	}
	if took := time.Since(start); took > opts.RevocationBound() {
		t.Errorf("the stream outlived the demotion by %s, bound %s", took, opts.RevocationBound())
	}
}

func TestAStreamClosesAtTheThirdFailedRecheckAndWithinTheFaultBound(t *testing.T) {
	fail := new(atomic.Bool)
	streamDBWrap = func(d *fake.DB) store.Database { return readFailDB{d, fail} }
	t.Cleanup(func() { streamDBWrap = nil })
	opts := StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}
	st := newStreamTable(t, opts, 0)
	c, _ := st.open(t, "user-two", "")

	fail.Store(true)
	start := time.Now()
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Fatalf("close code = %d, want %d", got, CloseMembershipChanged)
	}
	took := time.Since(start)
	if took < (maxRecheckFailures-1)*testRecheck {
		t.Errorf("closed after %s, before the third failed re-check (at least %s)", took, (maxRecheckFailures-1)*testRecheck)
	}
	if took > opts.FaultBound() {
		t.Errorf("closed after %s, want within the fault bound %s", took, opts.FaultBound())
	}
}

func TestAStreamSurvivesFewerThanThreeFailedRechecksInARow(t *testing.T) {
	fail := new(atomic.Bool)
	streamDBWrap = func(d *fake.DB) store.Database { return readFailDB{d, fail} }
	t.Cleanup(func() { streamDBWrap = nil })
	st := newStreamTable(t, StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}, 0)
	c, _ := st.open(t, "user-two", "")
	frame(t, c) // the head frame that follows the hello

	for range 3 { // a failure, then a success, repeatedly: never three in a row
		fail.Store(true)
		time.Sleep(testRecheck + testRecheck/2)
		fail.Store(false)
		time.Sleep(3 * testRecheck)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the stream ended after isolated failed re-checks: %v", err)
	}
}

func TestTheRecheckIntervalNeverExceedsTheHeartbeatAndTheBoundIsStated(t *testing.T) {
	o := StreamOptions{Heartbeat: 3 * time.Second, RecheckInterval: time.Minute}
	if o.recheck() != 3*time.Second {
		t.Errorf("recheck = %s, want it clamped to the heartbeat", o.recheck())
	}
	if d := (StreamOptions{}).RevocationBound(); d != 7*time.Second {
		t.Errorf("default revocation bound = %s, want 7s (5s interval + 2s check timeout)", d)
	}
	if d := (StreamOptions{}).FaultBound(); d != 21*time.Second {
		t.Errorf("default fault bound = %s, want 21s", d)
	}
}
