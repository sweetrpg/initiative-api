package startup

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

func fastPolicy(budget time.Duration, slept *[]time.Duration) Policy {
	p := DefaultPolicy()
	p.Budget = budget
	now := time.Unix(1000, 0)
	p.Now = func() time.Time { return now }
	p.Sleep = func(d time.Duration) { *slept = append(*slept, d); now = now.Add(d) }
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

func TestTheBudgetCountsTheTimeAttemptsTakeNotJustTheWaits(t *testing.T) {
	now := time.Unix(1000, 0)
	p := DefaultPolicy()
	p.Budget = 30 * time.Second
	p.Now = func() time.Time { return now }
	p.Sleep = func(d time.Duration) { now = now.Add(d) }
	tries := 0
	err := Do("connect", p, func() error { tries++; now = now.Add(12 * time.Second); return errors.New("timeout") })
	if err == nil || tries > 3 {
		t.Errorf("err=%v tries=%d: attempts of 12s each should end the 30s budget after two or three tries", err, tries)
	}
}

func TestACredentialInAnErrorNeverReachesTheReturnedText(t *testing.T) {
	boom := errors.New(`parse "mongodb+srv://appuser:s3cretpw@cluster.example.test/db": invalid port`)
	var slept []time.Duration
	err := Do("connect to the database", fastPolicy(time.Second, &slept), func() error { return boom })
	if err == nil || strings.Contains(err.Error(), "s3cretpw") || !strings.Contains(err.Error(), "://***@cluster") {
		t.Errorf("err = %v, want the password redacted", err)
	}
	if !errors.Is(err, boom) {
		t.Error("redaction broke errors.Is")
	}
}

func TestAZeroFirstDelayIsFlooredSoARetryNeverSpins(t *testing.T) {
	var slept []time.Duration
	p := fastPolicy(5*time.Second, &slept)
	p.First = 0
	_ = Do("x", p, func() error { return errors.New("down") })
	if len(slept) == 0 || slept[0] < 100*time.Millisecond || len(slept) > 20 {
		t.Errorf("waits = %v", slept)
	}
}

func TestOnlyARealIndexConflictIsPermanent(t *testing.T) {
	if !IsIndexConflict(fmt.Errorf("ensure indexes: %w", mongo.CommandError{Code: 85, Name: "IndexOptionsConflict"})) {
		t.Error("code 85 should be permanent")
	}
	if !IsIndexConflict(mongo.CommandError{Code: 86}) {
		t.Error("code 86 should be permanent")
	}
	if IsIndexConflict(mongo.CommandError{Code: 6, Name: "HostUnreachable"}) || IsIndexConflict(errors.New("timeout")) {
		t.Error("a transient failure is not permanent")
	}
}
