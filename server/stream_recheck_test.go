package server

import (
	"context"
	"errors"
	"net/http"
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
	for name, tk := range map[string]string{"a ticket minted earlier": old, "a ticket minted during the outage": fresh} {
		if tk == "" || (name != "a ticket minted earlier" && code != http.StatusCreated) {
			continue
		}
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
