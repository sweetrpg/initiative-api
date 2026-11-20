// Package startup holds the retry that process start-up uses against its dependencies. A pod that
// starts while the cluster resolver or a dependency is briefly unreachable should wait it out, not
// crash-loop: the dependency is usually back within seconds.
package startup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sweetrpg/common.go/logging"
	"go.mongodb.org/mongo-driver/mongo"
)

// Policy bounds a retry. Attempts are spaced by a delay that doubles from First up to Max, and stop
// once the time spent plus the next wait would pass Budget (attempt time counts, not only the
// waits). An error Permanent reports (an authentication failure, an index conflict) will not fix
// itself, so it gets PermanentAfter tries in all.
type Policy struct {
	Budget, First, Max time.Duration
	PermanentAfter     int
	Permanent          func(error) bool
	// Sleep and Now are time.Sleep and time.Now unless a test replaces them.
	Sleep func(time.Duration)
	Now   func() time.Time
}

// DefaultPolicy is a minute of retries, 1s growing to 10s, and two tries for an authentication
// failure.
func DefaultPolicy() Policy {
	return Policy{Budget: time.Minute, First: time.Second, Max: 10 * time.Second, PermanentAfter: 2, Permanent: IsAuthError, Sleep: time.Sleep, Now: time.Now}
}

// IndexPolicy is p for creating indexes: a conflict with an existing index never succeeds on
// retry, so it fails on the first try.
func IndexPolicy(p Policy) Policy {
	p.Permanent, p.PermanentAfter = IsIndexConflict, 1
	return p
}

// Do runs attempt until it succeeds, retrying as p says. Each failed attempt is logged at warn with
// what was being done; the last error is returned once the budget is spent. Connection URIs in an
// error (user:password@host) are masked in both the log and the returned error.
func Do(what string, p Policy, attempt func() error) error {
	if p.Sleep == nil {
		p.Sleep = time.Sleep
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.First <= 0 {
		p.First = time.Second // a zero delay would loop without waiting
	}
	p.Max = max(p.Max, p.First)
	start := p.Now()
	backoff := p.First
	var permanent int
	for n := 1; ; n++ {
		err := attempt()
		if err == nil {
			return nil
		}
		if p.Permanent != nil && p.Permanent(err) {
			if permanent++; permanent >= max(p.PermanentAfter, 1) {
				return redacted(fmt.Sprintf("%s failed after %d tries and will not fix itself: ", what, permanent), err)
			}
		}
		if elapsed := p.Now().Sub(start); elapsed+backoff > p.Budget {
			return redacted(fmt.Sprintf("%s failed after %d tries over %s: ", what, n, elapsed.Round(time.Millisecond)), err)
		}
		logging.Logger.Warn("startup step failed; retrying", "step", what, "attempt", n, "retry_in", backoff.String(), "error", Redact(err.Error()))
		p.Sleep(backoff)
		backoff = min(backoff*2, p.Max)
	}
}

var uriUserinfo = regexp.MustCompile(`://[^/@\s]*@`)

// Redact masks the user:password part of any connection URI in s.
func Redact(s string) string { return uriUserinfo.ReplaceAllString(s, "://***@") }

// redactedError carries a masked message and still unwraps to the original, so errors.Is and As
// work on it.
type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }

func redacted(prefix string, err error) error {
	return redactedError{msg: prefix + Redact(err.Error()), err: err}
}

// IsAuthError reports whether err looks like a rejected credential, which waiting does not fix.
func IsAuthError(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "authenticationfailed") || strings.Contains(m, "authentication failed")
}

// MongoIndexOptionsConflict and MongoIndexKeySpecsConflict are the server's answers to creating an
// index that differs from an existing one of the same name or keys (a changed TTL, say). They
// only go away when someone drops or changes the index.
const (
	MongoIndexOptionsConflict  = 85
	MongoIndexKeySpecsConflict = 86
)

// IsIndexConflict reports whether err is an index definition conflict.
func IsIndexConflict(err error) bool {
	var ce mongo.CommandError
	return errors.As(err, &ce) && (ce.HasErrorCode(MongoIndexOptionsConflict) || ce.HasErrorCode(MongoIndexKeySpecsConflict))
}
