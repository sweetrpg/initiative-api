package startup

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fastPolicy(budget time.Duration, slept *[]time.Duration) Policy {
	p := DefaultPolicy()
	p.Budget = budget
	p.Sleep = func(d time.Duration) { *slept = append(*slept, d) }
	return p
}

func TestDoRetriesUntilTheStepSucceedsWithADoublingBackoffCappedAtMax(t *testing.T) {
	var slept []time.Duration
	n := 0
	err := Do("create indexes", fastPolicy(5*time.Minute, &slept), func() error {
		if n++; n <= 6 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil || n != 7 {
		t.Fatalf("err=%v tries=%d, want success on the seventh", err, n)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, d := range want {
		if slept[i] != d {
			t.Fatalf("waits = %v, want %v", slept, want)
		}
	}
}

func TestDoNamesTheStepAndWrapsTheLastErrorWhenTheBudgetIsSpent(t *testing.T) {
	var slept []time.Duration
	boom := errors.New("connection refused")
	err := Do("start the auth-event consumer", fastPolicy(10*time.Second, &slept), func() error { return boom })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "start the auth-event consumer") {
		t.Errorf("err = %v", err)
	}
}

func TestDoGivesAPermanentErrorOnlyItsFewTries(t *testing.T) {
	var slept []time.Duration
	n := 0
	err := Do("connect", fastPolicy(time.Hour, &slept), func() error { n++; return errors.New("AuthenticationFailed") })
	if err == nil || n != 2 {
		t.Errorf("err=%v tries=%d, want 2 tries", err, n)
	}
}
