package stream

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

// stuckPrefs blocks every read until release is closed, ignoring the caller's context, as a store
// that has stopped answering does.
type stuckPrefs struct{ release chan struct{} }

func (p stuckPrefs) Muted(context.Context, string, string) ([]string, error) {
	<-p.release
	return nil, nil
}

func note() data.Notice {
	return data.Notice{Type: data.NoticeTurnYours, SessionID: "s", EncounterID: "e", Users: []string{"u-1"}, At: time.Now()}
}

func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

func TestAFullNoticeQueueDropsAndCountsInsteadOfBlocking(t *testing.T) {
	h := New(nil)
	release := make(chan struct{})
	h.SetPreferences(stuckPrefs{release})
	within(t, "queueing past capacity", func() {
		for range noticeQueue + 4 { // the worker holds one; the queue holds noticeQueue
			h.Notify(context.Background(), note())
		}
	})
	if got := h.Dropped(); got < 3 {
		t.Errorf("dropped %d notices after overfilling the queue, want at least 3", got)
	}
	close(release)
	within(t, "Flush", h.Flush)
	h.Close()
}

func TestAfterCloseNoticesAreDroppedAndCountedAndFlushReturns(t *testing.T) {
	h := New(nil)
	h.Notify(context.Background(), note())
	h.Flush()
	h.Close()
	before := h.Dropped()
	h.Notify(context.Background(), note())
	if h.Dropped() != before+1 {
		t.Errorf("dropped %d -> %d, want a notice after Close counted as dropped", before, h.Dropped())
	}
	within(t, "Flush after Close", h.Flush)
}

func TestClosingBeforeTheFirstNoticeNeverStartsAWorker(t *testing.T) {
	h := New(nil)
	h.Close()
	h.Notify(context.Background(), note())
	if h.noticeQ != nil {
		t.Error("a worker was started on a closed hub")
	}
	if h.Dropped() != 1 {
		t.Errorf("dropped = %d, want the notice counted", h.Dropped())
	}
	within(t, "Flush", h.Flush)
}

func TestCloseWithNoticesStillQueuedLeavesNothingPending(t *testing.T) {
	h := New(nil)
	release := make(chan struct{})
	h.SetPreferences(stuckPrefs{release})
	for range 10 {
		h.Notify(context.Background(), note())
	}
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	time.Sleep(20 * time.Millisecond) // Close is now waiting on the stuck worker
	close(release)
	within(t, "Close", func() { <-closed })
	within(t, "Flush", h.Flush)
}

func TestNoticesStillQueuedWhenTheHubClosesAreCountedExactlyOnce(t *testing.T) {
	h := New(nil)
	release := make(chan struct{})
	h.SetPreferences(stuckPrefs{release})
	for range 11 {
		h.Notify(context.Background(), note())
	}
	waitUntil(t, func() bool { h.mu.RLock(); defer h.mu.RUnlock(); return len(h.noticeQ) == 10 }) // one is in the worker
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	waitUntil(t, func() bool { h.mu.RLock(); defer h.mu.RUnlock(); return h.noticeClosed })
	before := h.Dropped()
	close(release)
	within(t, "Close", func() { <-closed })
	if got := h.Dropped() - before; got != 10 {
		t.Errorf("the drain counted %d discarded notices, want the 10 still queued", got)
	}
	within(t, "Flush", h.Flush)
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// slowPrefs honors its context and counts reads.
type slowPrefs struct{ calls atomic.Int32 }

func (p *slowPrefs) Muted(ctx context.Context, _, _ string) ([]string, error) {
	p.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAFailedMuteReadIsLoggedOnceAndTheRestAreSkipped(t *testing.T) {
	old := muteLookupTimeout
	muteLookupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { muteLookupTimeout = old })
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := New(nil)
	p := &slowPrefs{}
	h.SetPreferences(p)

	n := note()
	n.Users = []string{"u-1", "u-2", "u-3"}
	start := time.Now()
	muted := h.mutedUsers(n)

	if len(muted) != 0 {
		t.Errorf("muted = %v, want none (fail open)", muted)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("%d preference reads, want 1: the rest are skipped after the first failure", got)
	}
	if time.Since(start) > time.Second {
		t.Error("the lookups were not bounded by their own timeout")
	}
	if out := buf.String(); strings.Count(out, "reading notification preferences failed") != 1 || !strings.Contains(out, "unchecked=3") || !strings.Contains(out, "level=WARN") {
		t.Errorf("log = %q, want one WARN with unchecked=3", out)
	}
}

func TestAPrivateEncounterEvictionLeavesTheUsersUserLevelStreamOpen(t *testing.T) {
	h := New(nil)
	private := h.Open("", "u-1", "", 4)
	userLevel := h.OpenUser("u-1", 4)
	h.evictAudience(data.Change{OwnerID: "u-1"})
	if !private.Ended() {
		t.Error("the private-encounter stream survived its audience's eviction")
	}
	if userLevel.Ended() {
		t.Error("a private-encounter eviction closed the user-level stream")
	}
}

type countingMembers struct{ calls atomic.Int32 }

func (m *countingMembers) IsMember(context.Context, string, string) (bool, error) {
	m.calls.Add(1)
	return true, nil
}

type nopBus struct{ got atomic.Int32 }

func (b *nopBus) Publish(context.Context, Event) error { b.got.Add(1); return nil }

func TestOneMembershipReadServesBothTheStreamAndTheBus(t *testing.T) {
	h := New(nil)
	m := &countingMembers{}
	bus := &nopBus{}
	h.SetMembers(m)
	h.AttachBus(bus, 4)
	t.Cleanup(h.Close)
	sub := h.OpenUser("u-1", 4)

	h.Notify(context.Background(), note())
	h.Flush()

	waitUntil(t, func() bool { return bus.got.Load() == 1 })
	if len(sub.C) != 1 {
		t.Fatalf("the user-level stream got %d notices, want 1", len(sub.C))
	}
	if got := m.calls.Load(); got != 1 {
		t.Errorf("%d membership reads for one addressee, want 1 shared by the stream and the bus", got)
	}
}
