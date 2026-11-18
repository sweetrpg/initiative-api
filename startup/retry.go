// Package startup holds the retry that process start-up uses against its dependencies. A pod that
// starts while the cluster resolver or a dependency is briefly unreachable should wait it out, not
// crash-loop: the dependency is usually back within seconds.
package startup

import (
	"fmt"
	"strings"
	"time"

	"github.com/sweetrpg/common.go/logging"
)

// Policy bounds a retry. Attempts are spaced by a delay that doubles from First up to Max, and stop
// once the next wait would pass Budget. An error Permanent reports (an authentication failure, say)
// will not fix itself, so it gets PermanentAfter tries in all.
type Policy struct {
	Budget, First, Max time.Duration
	PermanentAfter     int
	Permanent          func(error) bool
	// Sleep is time.Sleep unless a test replaces it.
	Sleep func(time.Duration)
}

// DefaultPolicy is a minute of retries, 1s growing to 10s, and two tries for an authentication
// failure.
func DefaultPolicy() Policy {
	return Policy{Budget: time.Minute, First: time.Second, Max: 10 * time.Second, PermanentAfter: 2, Permanent: IsAuthError, Sleep: time.Sleep}
}

// Do runs attempt until it succeeds, retrying as p says. Each failed attempt is logged at warn with
// what was being done; the last error is returned once the budget is spent.
func Do(what string, p Policy, attempt func() error) error {
	if p.Sleep == nil {
		p.Sleep = time.Sleep
	}
	var waited time.Duration
	backoff := p.First
	var permanent int
	for n := 1; ; n++ {
		err := attempt()
		if err == nil {
			return nil
		}
		if p.Permanent != nil && p.Permanent(err) {
			if permanent++; permanent >= max(p.PermanentAfter, 1) {
				return fmt.Errorf("%s failed after %d tries and will not fix itself: %w", what, permanent, err)
			}
		}
		if waited+backoff > p.Budget {
			return fmt.Errorf("%s failed after %d tries over %s: %w", what, n, waited, err)
		}
		logging.Logger.Warn("startup step failed; retrying", "step", what, "attempt", n, "retry_in", backoff.String(), "error", err.Error())
		p.Sleep(backoff)
		waited += backoff
		backoff = min(backoff*2, p.Max)
	}
}

// IsAuthError reports whether err looks like a rejected credential, which waiting does not fix.
func IsAuthError(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "authenticationfailed") || strings.Contains(m, "authentication failed") || strings.Contains(m, "auth error")
}
