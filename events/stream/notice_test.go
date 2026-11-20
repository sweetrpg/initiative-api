package stream

import (
	"context"
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
