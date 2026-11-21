package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// streamDBWrap, when set by a test, wraps the database newStreamTable builds the router on.
var streamDBWrap func(*fake.DB) store.Database

// readFault injects session-store read failures: while down is set every read of the sessions
// collection fails, and failN fails the next N reads. rechecks counts the reads, one per membership
// re-check, so a test drives failures by count and not by sleeping.
type readFault struct {
	down     atomic.Bool
	failN    atomic.Int32
	rechecks atomic.Int32
}

type readFailDB struct {
	*fake.DB
	f *readFault
}

func (d readFailDB) Collection(name string) store.Collection {
	c := d.DB.Collection(name)
	if name != data.CollectionSessions {
		return c
	}
	return readFailCollection{c, d.f}
}

type readFailCollection struct {
	store.Collection
	f *readFault
}

func (c readFailCollection) FindOne(ctx context.Context, filter bson.D, out any) error {
	c.f.rechecks.Add(1)
	if c.f.down.Load() {
		return errors.New("mongo: server selection timeout")
	}
	for {
		n := c.f.failN.Load()
		if n <= 0 {
			break
		}
		if c.f.failN.CompareAndSwap(n, n-1) {
			return errors.New("mongo: server selection timeout")
		}
	}
	return c.Collection.FindOne(ctx, filter, out)
}

func useReadFault(t *testing.T) *readFault {
	t.Helper()
	f := &readFault{}
	streamDBWrap = func(d *fake.DB) store.Database { return readFailDB{d, f} }
	t.Cleanup(func() { streamDBWrap = nil })
	return f
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
	fault := useReadFault(t)
	opts := StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}
	st := newStreamTable(t, opts, 0)
	c, _ := st.open(t, "user-two", "")

	base := fault.rechecks.Load()
	fault.down.Store(true)
	start := time.Now()
	if got := closeCode(c); got != CloseMembershipChanged {
		t.Fatalf("close code = %d, want %d", got, CloseMembershipChanged)
	}
	took := time.Since(start)
	if n := fault.rechecks.Load() - base; n != maxRecheckFailures {
		t.Errorf("the stream closed after %d re-checks, want exactly %d failed ones", n, maxRecheckFailures)
	}
	if took > opts.FaultBound() {
		t.Errorf("closed after %s, want within the fault bound %s", took, opts.FaultBound())
	}
}

func TestAStreamSurvivesFewerThanThreeFailedRechecksInARow(t *testing.T) {
	fault := useReadFault(t)
	st := newStreamTable(t, StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}, 0)
	c, _ := st.open(t, "user-two", "")
	frame(t, c) // the head frame that follows the hello

	for range 3 { // two failures, then two good re-checks, repeatedly: never three failures in a row
		base := fault.rechecks.Load()
		fault.failN.Store(2)
		waitFor(t, "two failed and two good re-checks", func() bool { return fault.rechecks.Load() >= base+4 })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the stream ended after isolated failed re-checks: %v", err)
	}
}

func TestWhileTheStoreIsDownANewTicketAndAnOldOneAreBothRefused(t *testing.T) {
	fault := useReadFault(t)
	st := newStreamTable(t, StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck}, 0)
	old, _ := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)

	fault.down.Store(true)

	// Minting is authorized through OpenFGA, which is up here; the connect re-reads the member's
	// record, so whichever ticket carries the client to the door, the connect is refused.
	fresh, code := st.ticket(t, "user-two", `{"session_id":"`+st.sid+`"}`)
	if code != http.StatusCreated || fresh == "" {
		t.Fatalf("minting during the outage = %d; this test needs it to succeed (OpenFGA is up) to exercise the connect", code)
	}
	for name, tk := range map[string]string{"a ticket minted earlier": old, "a ticket minted during the outage": fresh} {
		if got := st.refused(t, "ticket="+tk, nil); got == http.StatusSwitchingProtocols || got < http.StatusInternalServerError {
			t.Errorf("a stream opened with %s = %d, want a 5xx refusal", name, got)
		}
	}
}

func TestTheRecheckIntervalNeverExceedsTheHeartbeatAndTheBoundsAreStated(t *testing.T) {
	o := StreamOptions{Heartbeat: 3 * time.Second, RecheckInterval: time.Minute}
	if o.recheck() != 3*time.Second {
		t.Errorf("recheck = %s, want it clamped to the heartbeat", o.recheck())
	}
	d := StreamOptions{}
	for name, got := range map[string][2]time.Duration{
		"revocation bound": {d.RevocationBound(), 7 * time.Second}, // 5s interval + 2s check
		"stalled bound":    {d.StalledBound(), 19 * time.Second},   // + 10s write + 2s renewal
		"fault bound":      {d.FaultBound(), 21 * time.Second},     // 3 x 7s
		"stalled fault":    {d.StalledFaultBound(), 33 * time.Second},
	} {
		if got[0] != got[1] {
			t.Errorf("default %s = %s, want %s", name, got[0], got[1])
		}
	}
}

// A write backlog must not postpone the re-check. While the loop is held in the first event's
// write the re-check falls due; when the write ends, the next iteration finds both the next event
// and the re-check ready, and select alone would pick between them at random. The re-check must go
// first every time, so no second event is written. Several trials make the random pick fail a
// version without the priority almost surely (1 - 2^-trials) and never fail the real one.
func TestADueRecheckGoesBeforeTheNextBufferedEventWrite(t *testing.T) {
	for trial := range 8 {
		var writes atomic.Int32
		release := make(chan struct{})
		opts := StreamOptions{Heartbeat: time.Hour, RecheckInterval: testRecheck, beforeWrite: func() {
			if writes.Add(1) == 1 {
				<-release // hold the loop in the first event's write
			}
		}}
		st := newStreamTable(t, opts, 0)
		st.svc.Sessions.SetEvictor(nil)
		c, _ := st.open(t, "user-two", "")
		for range 3 {
			st.hub.Emit(context.Background(), data.Change{Type: data.EventCombatantUpdated, After: userStreamEncounter(t, st), SessionID: st.sid, OwnerID: "u-1", CombatantID: []string{"orc"}, At: time.Now()})
		}
		waitFor(t, "the loop to be held in the first write", func() bool { return writes.Load() == 1 })
		if w := callAs(st.r, "user-ok", "DELETE", "/sessions/"+st.sid+"/members/u-2", ""); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
			t.Fatalf("remove = %d %s", w.Code, w.Body)
		}
		time.Sleep(3 * testRecheck) // the re-check is now due, and the loop has not taken it
		close(release)
		if got := closeCode(c); got != CloseMembershipChanged {
			t.Fatalf("trial %d: close code = %d, want %d", trial, got, CloseMembershipChanged)
		}
		if n := writes.Load(); n != 1 {
			t.Fatalf("trial %d: %d event writes began, want 1: a due re-check went behind a buffered event", trial, n)
		}
	}
}

// slowLeases blocks every seat write until its context ends, as a hung store does, once slow is set.
type slowLeases struct {
	*fake.DB
	slow *atomic.Bool
}

func (d slowLeases) Collection(name string) store.Collection {
	c := d.DB.Collection(name)
	if name != data.CollectionStreamLeases {
		return c
	}
	return slowLeaseCollection{c, d.slow}
}

type slowLeaseCollection struct {
	store.Collection
	slow *atomic.Bool
}

func (c slowLeaseCollection) FindOneAndUpdate(ctx context.Context, f, u bson.D, out any) error {
	if c.slow.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.Collection.FindOneAndUpdate(ctx, f, u, out)
}

func TestASeatRenewalIsCappedAtTheCheckTimeoutNotHalfTheHeartbeat(t *testing.T) {
	slow := new(atomic.Bool)
	streamDBWrap = func(d *fake.DB) store.Database { return slowLeases{d, slow} }
	t.Cleanup(func() { streamDBWrap = nil })
	renewed := make(chan time.Time, 4)
	opts := StreamOptions{ConcurrentMax: 5, Heartbeat: 6 * time.Second, LeaseTTL: time.Minute, afterRenew: func() { renewed <- time.Now() }}
	st := newStreamTable(t, opts, 0)
	c, _ := st.open(t, "user-two", "")
	frame(t, c) // the head frame
	slow.Store(true)

	rctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	for { // the heartbeat frame is written just before the renewal starts
		_, b, err := c.Read(rctx)
		if err != nil {
			t.Fatalf("waiting for the heartbeat: %v", err)
		}
		if strings.Contains(string(b), `"heartbeat"`) {
			break
		}
	}
	beat := time.Now()
	select {
	case at := <-renewed:
		if took := at.Sub(beat); took > checkTimeout+500*time.Millisecond {
			t.Errorf("a hung seat store held the loop for %s, want it cut at the check timeout (%s), not heartbeat/2 (%s)", took, checkTimeout, opts.Heartbeat/2)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the seat renewal never returned")
	}
}
