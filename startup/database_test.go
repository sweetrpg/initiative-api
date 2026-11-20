package startup

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweetrpg/common.go/logging"
)

func TestMain(m *testing.M) {
	logging.Init()
	os.Exit(m.Run())
}

type script struct {
	setupErrs []error // returned in order, then nil
	pingErrs  []error
	setups    int
	pings     int
	teardowns int
	slept     []time.Duration
}

func (s *script) conn() DB {
	return DB{
		Setup: func() error {
			i := s.setups
			s.setups++
			if i < len(s.setupErrs) {
				return s.setupErrs[i]
			}
			return nil
		},
		Ping: func(context.Context) error {
			i := s.pings
			s.pings++
			if i < len(s.pingErrs) {
				return s.pingErrs[i]
			}
			return nil
		},
		Teardown: func() { s.teardowns++ },
	}
}

func (s *script) policy(budget time.Duration) Policy {
	p := DefaultPolicy()
	p.Budget = budget
	now := time.Unix(1000, 0)
	p.Now = func() time.Time { return now }
	p.Sleep = func(d time.Duration) { s.slept = append(s.slept, d); now = now.Add(d) }
	return p
}

var errDNS = errors.New(`error parsing uri: lookup _mongodb._tcp.cluster0.example.test on 10.0.0.10:53: read udp: i/o timeout`)

func TestAResolveFailureIsRetriedWithBackoffUntilItSucceeds(t *testing.T) {
	s := &script{setupErrs: []error{errDNS, errDNS, errDNS}}
	if err := ConnectDB(s.conn(), s.policy(time.Minute)); err != nil {
		t.Fatalf("connect = %v, want success on the fourth try", err)
	}
	if s.setups != 4 || len(s.slept) != 3 {
		t.Errorf("setups=%d sleeps=%v, want 4 tries and 3 waits", s.setups, s.slept)
	}
	if s.slept[0] != time.Second || s.slept[1] != 2*time.Second || s.slept[2] != 4*time.Second {
		t.Errorf("backoff = %v, want 1s, 2s, 4s", s.slept)
	}
}

func TestAnUnreachableServerIsCaughtByThePingAndTheClientIsClosedBeforeTheRetry(t *testing.T) {
	s := &script{pingErrs: []error{errors.New("server selection timeout")}}
	if err := ConnectDB(s.conn(), s.policy(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s.teardowns != 1 || s.setups != 2 {
		t.Errorf("teardowns=%d setups=%d, want the failed client closed once and a second setup", s.teardowns, s.setups)
	}
}

func TestTheRetriesStopWhenTheBudgetIsSpentAndTheLastErrorIsReturned(t *testing.T) {
	s := &script{setupErrs: make([]error, 100)}
	for i := range s.setupErrs {
		s.setupErrs[i] = errDNS
	}
	err := ConnectDB(s.conn(), s.policy(20*time.Second))
	if err == nil || !errors.Is(err, errDNS) || !strings.Contains(err.Error(), "failed after") {
		t.Fatalf("err = %v, want the unreachable error wrapping the last failure", err)
	}
	var total time.Duration
	for _, d := range s.slept {
		total += d
	}
	if total > 20*time.Second {
		t.Errorf("waited %s, more than the 20s budget", total)
	}
	if s.setups < 3 {
		t.Errorf("only %d tries", s.setups)
	}
}

func TestAnAuthenticationFailureGetsTwoTriesNotTheWholeBudget(t *testing.T) {
	bad := errors.New("(AuthenticationFailed) Authentication failed.")
	s := &script{setupErrs: []error{bad, bad, bad, bad}}
	err := ConnectDB(s.conn(), s.policy(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "after 2 tries") {
		t.Fatalf("err = %v", err)
	}
	if s.setups != 2 {
		t.Errorf("setups = %d, want 2", s.setups)
	}
}

func TestAnUnparsableURIFromTheEnvironmentIsAnErrorWithNoPasswordNotAPanic(t *testing.T) {
	t.Setenv("DB_URI", "mongodb://appuser:s3cretpw@host:notaport/db")
	var slept []time.Duration
	p := DefaultPolicy()
	p.Budget = time.Second
	p.Sleep = func(d time.Duration) { slept = append(slept, d) }
	err := ConnectDatabase(p)
	if err == nil {
		t.Fatal("a bad URI connected")
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Errorf("the error carries the password: %v", err)
	}
}
