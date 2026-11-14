package reconcile

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gomodule/redigo/redis"
)

const (
	// tokenCacheKey holds the cached Auth0 access token (PADR-0041).
	tokenCacheKey = "reconciler:token"
	// tokenSkew is how long before expiry a token stops being served from the cache.
	tokenSkew = 120 * time.Second
	// rejectedKey marks that auth-api rejected a freshly fetched token. While it exists no run
	// calls Auth0, so a persistent 401 (role removed, broken login mapping, wrong audience) cannot
	// spend the monthly token allowance. Delete it to retry sooner.
	rejectedKey = "reconciler:rejected"
	rejectedTTL = time.Hour
	// fetchGuardKey is set (NX, 60s) before every Auth0 call. A Redis that cannot take writes then
	// stops the run before Auth0 is called, and a working one limits fetches to one a minute, so a
	// cache that silently fails to store cannot cost a token per run.
	fetchGuardKey = "reconciler:fetch-guard"
	fetchGuardTTL = 60 * time.Second
	// auth0FailuresKey counts consecutive failed token fetches (TTL 2h, reset on success) and
	// auth0BackoffKey blocks fetching while it exists, for 5m * 2^(failures-1) capped at 1h. After it
	// expires the next fetch is the probe: success clears both keys, failure re-arms the backoff.
	// A cached token is still served during backoff; only Auth0 calls stop.
	auth0FailuresKey = "reconciler:auth0-failures"
	auth0BackoffKey  = "reconciler:auth0-backoff"
	failuresTTL      = 2 * time.Hour
	backoffBase      = 5 * time.Minute
	backoffMax       = time.Hour
	// maxLifetime caps a lifetime Auth0 reports, so a bad value cannot cache a token for years.
	maxLifetime = 30 * 24 * time.Hour
)

// TokenSource supplies the bearer the reconciler sends to auth-api.
type TokenSource interface {
	// Token returns a usable access token.
	Token(ctx context.Context) (string, error)
	// Refresh discards the current token after auth-api answered 401 and returns a new one. It
	// returns ErrNoRefresh when a replacement cannot help, such as a token fetched this run.
	Refresh(ctx context.Context) (string, error)
	// Rejected records that auth-api rejected a token fetched this run, so later runs stop. It
	// returns an error when the record could not be written.
	Rejected(ctx context.Context) error
	// Accepted records that auth-api accepted the token, ending any earlier rejection.
	Accepted(ctx context.Context)
}

// ErrAuth0Backoff is returned while earlier token fetches failed and the backoff has not expired.
// It is not a token rejection: the run fails with exit 1 and Auth0 is not called.
var ErrAuth0Backoff = errors.New("reconcile: Auth0 token fetch is backing off after earlier failures")

// ErrNoRefresh is returned by a source that cannot replace a rejected token.
var ErrNoRefresh = errors.New("reconcile: token cannot be refreshed")

// StaticToken is a TokenSource for one fixed token.
type StaticToken string

// Token returns the fixed token.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// Refresh always fails: a fixed token has no replacement.
func (s StaticToken) Refresh(context.Context) (string, error) { return "", ErrNoRefresh }

// Rejected does nothing for a fixed token.
func (s StaticToken) Rejected(context.Context) error { return nil }

// Accepted does nothing for a fixed token.
func (s StaticToken) Accepted(context.Context) {}

// ClientCredentials fetches an Auth0 machine-to-machine access token with the client-credentials
// grant and caches it in Redis until 120 seconds before it expires, so a job that runs every five
// minutes uses about 30 Auth0 tokens a month instead of 8,640 (PADR-0041). The secret is never
// logged or put in an error.
type ClientCredentials struct {
	TokenURL, ClientID, ClientSecret, Audience string
	Pool                                       *redis.Pool
	HTTP                                       *http.Client

	token    string // this run's token, so one run reads Redis once
	err      error  // sticky: one failed fetch fails the rest of the run without hitting Auth0 again
	fetched  bool   // the token came from Auth0 in this run, so a 401 on it is final
	accepted bool   // the rejection marker was already cleared this run
}

// NewClientCredentials returns a source that caches in pool.
func NewClientCredentials(tokenURL, clientID, clientSecret, audience string, pool *redis.Pool) *ClientCredentials {
	return &ClientCredentials{TokenURL: tokenURL, ClientID: clientID, ClientSecret: clientSecret, Audience: audience,
		Pool: pool, HTTP: &http.Client{
			Timeout: callTimeout,
			// A redirect would re-send the POST body, client secret included, to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
}

// Token returns the cached token, or fetches and caches a new one.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.token != "" {
		return c.token, nil
	}
	tok, err := c.rejectionOrCached()
	if err == nil && tok == "" {
		tok, err = c.fetchAndStore(ctx)
	}
	return c.remember(tok, err)
}

// rejectionOrCached fails with ErrTokenRejected while the rejection marker exists, and otherwise
// returns the cached token ("" when there is none).
func (c *ClientCredentials) rejectionOrCached() (string, error) {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	marked, err := redis.Bool(conn.Do("EXISTS", rejectedKey))
	if err != nil {
		return "", fmt.Errorf("read rejection marker: %w", err)
	}
	if marked {
		return "", fmt.Errorf("auth-api rejected the last fresh token, not calling Auth0 until %s expires: %w", rejectedKey, ErrTokenRejected)
	}
	return c.cached()
}

// Refresh clears the cached token and fetches a new one.
func (c *ClientCredentials) Refresh(ctx context.Context) (string, error) {
	if c.fetched {
		return "", ErrNoRefresh // a token fetched this run was rejected; another would be too
	}
	// While Auth0 is backing off no replacement can be fetched, so keep the cached token and set no
	// rejection marker: the run fails with exit 1 and the next one reuses the cache.
	if err := c.checkBackoff(ctx); err != nil {
		return c.remember("", err)
	}
	c.token, c.err = "", nil
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	if _, err := conn.Do("DEL", tokenCacheKey); err != nil {
		return c.remember("", fmt.Errorf("clear cached token: %w", err))
	}
	tok, err := c.fetchAndStore(ctx)
	return c.remember(tok, err)
}

// Rejected clears the bad cached token and sets the rejection marker, so no run calls Auth0 for an
// hour. The fetch guard still limits Auth0 calls to one a minute if the marker cannot be written.
func (c *ClientCredentials) Rejected(context.Context) error {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	_ = conn.Send("DEL", tokenCacheKey)
	if _, err := conn.Do("SET", rejectedKey, "1", "EX", int64(rejectedTTL/time.Second)); err != nil {
		return fmt.Errorf("record token rejection: %w", err)
	}
	return nil
}

// Accepted deletes the rejection marker once per run.
func (c *ClientCredentials) Accepted(ctx context.Context) {
	if c.accepted {
		return
	}
	c.accepted = true
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	if _, err := conn.Do("DEL", rejectedKey); err != nil {
		slog.ErrorContext(ctx, "reconcile: could not clear the token rejection marker", "error", err)
	}
}

func (c *ClientCredentials) remember(tok string, err error) (string, error) {
	if err != nil {
		c.err = err
		return "", err
	}
	c.token = tok
	return tok, nil
}

// cached returns the token in Redis, or "" when there is none. Its TTL ends tokenSkew before the
// token does, so a hit is always usable.
func (c *ClientCredentials) cached() (string, error) {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	tok, err := redis.String(conn.Do("GET", tokenCacheKey))
	if errors.Is(err, redis.ErrNil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cached token: %w", err)
	}
	return tok, nil
}

func (c *ClientCredentials) fetchAndStore(ctx context.Context) (string, error) {
	if err := c.checkBackoff(ctx); err != nil {
		return "", err
	}
	if err := c.takeFetchGuard(); err != nil {
		return "", err
	}
	tok, lifetime, err := c.fetch(ctx)
	if err != nil {
		return "", c.recordFailure(ctx, err)
	}
	c.fetched = true
	defer c.clearFailures(ctx)
	ttl := lifetime - tokenSkew
	if ttl < time.Second {
		return tok, nil // too short to cache; the next run fetches again
	}
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	if _, err := conn.Do("SET", tokenCacheKey, tok, "EX", int64(ttl/time.Second)); err != nil {
		return "", fmt.Errorf("cache token: %w", err)
	}
	return tok, nil
}

// checkBackoff fails without calling Auth0 while the backoff key exists.
func (c *ClientCredentials) checkBackoff(ctx context.Context) error {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	left, err := redis.Int64(conn.Do("PTTL", auth0BackoffKey))
	if err != nil {
		return fmt.Errorf("read auth0 backoff: %w", err)
	}
	if left == -2 { // no key
		return nil
	}
	if left == -1 { // a key without expiry was set by hand; fail closed rather than guess
		slog.WarnContext(ctx, "reconcile: Auth0 backoff key has no expiry; not calling Auth0 until it is deleted")
		return fmt.Errorf("%w (key %s has no expiry; delete it)", ErrAuth0Backoff, auth0BackoffKey)
	}
	remaining := (time.Duration(left) * time.Millisecond).Round(time.Second)
	slog.WarnContext(ctx, "reconcile: Auth0 token fetch is backing off; not calling Auth0", "remaining", remaining.String())
	return fmt.Errorf("%w (retry in %v)", ErrAuth0Backoff, remaining)
}

// recordFailure counts a failed fetch and arms the backoff: 5m, 10m, 20m, 40m, then 1h, or Auth0's
// Retry-After when that is longer (capped at 1h). It returns the fetch error, joined with the Redis
// error when the backoff could not be written. A Redis that cannot take this write could not take
// the fetch guard's either, so the run would already have stopped before calling Auth0; the error
// is still returned so a failure here is never silent.
func (c *ClientCredentials) recordFailure(ctx context.Context, fetchErr error) error {
	if ctx.Err() != nil {
		return fetchErr // a run timeout or SIGTERM is not Auth0's fault
	}
	var retryAfter time.Duration
	var fe *fetchError
	if errors.As(fetchErr, &fe) {
		retryAfter = fe.retryAfter
	}
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	reply, err := redis.Int64s(recordFailureScript.Do(conn, auth0FailuresKey, auth0BackoffKey,
		int64(failuresTTL/time.Second), int64(backoffBase/time.Second), int64(backoffMax/time.Second), int64(retryAfter/time.Second)))
	if err != nil || len(reply) != 2 {
		return errors.Join(fetchErr, fmt.Errorf("arm auth0 backoff: %v", err))
	}
	slog.WarnContext(ctx, "reconcile: Auth0 token fetch failed; backing off", "failures", reply[0], "wait", (time.Duration(reply[1]) * time.Second).String())
	return fetchErr
}

// recordFailureScript counts a failure and arms the backoff in one atomic step, so a failure
// between two commands cannot leave a count with no TTL or a count with no backoff. KEYS: failures,
// backoff. ARGV: failures TTL, base, max, Retry-After (seconds). Returns {count, wait seconds}.
var recordFailureScript = redis.NewScript(2, `
local n = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
local wait = tonumber(ARGV[2]) * 2 ^ math.min(n - 1, 10)
local retry = tonumber(ARGV[4])
if retry > wait then wait = retry end
wait = math.floor(math.min(wait, tonumber(ARGV[3])))
redis.call('SET', KEYS[2], '1', 'EX', wait)
return {n, wait}
`)

// clearFailures ends the backoff after a successful fetch. A failure here only leaves keys that
// expire on their own, so it is logged, not returned.
func (c *ClientCredentials) clearFailures(ctx context.Context) {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	if _, err := conn.Do("DEL", auth0BackoffKey, auth0FailuresKey); err != nil {
		slog.ErrorContext(ctx, "reconcile: could not clear the Auth0 backoff keys", "error", err)
	}
}

// fetchError is a failed token request. retryAfter is Auth0's Retry-After in seconds, when sent on
// a 429.
type fetchError struct {
	msg        string
	retryAfter time.Duration
}

func (e *fetchError) Error() string { return e.msg }

// takeFetchGuard claims the right to call Auth0. It fails when Redis cannot take the write or
// another fetch happened in the last minute, in both cases before any Auth0 call.
func (c *ClientCredentials) takeFetchGuard() error {
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	reply, err := redis.String(conn.Do("SET", fetchGuardKey, "1", "NX", "EX", int64(fetchGuardTTL/time.Second)))
	if errors.Is(err, redis.ErrNil) {
		return fmt.Errorf("a token was fetched in the last %v; not calling Auth0 again", fetchGuardTTL)
	}
	if err != nil || reply != "OK" {
		return fmt.Errorf("fetch guard: redis cannot take writes, not calling Auth0: %v", err)
	}
	return nil
}

// fetch requests a token from Auth0. Errors carry the status, never the response body or secret.
func (c *ClientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type": "client_credentials", "client_id": c.ClientID, "client_secret": c.ClientSecret, "audience": c.Audience,
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, errors.New("build token request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Only the failure class: the URL error text could echo the request.
		return "", 0, &fetchError{msg: fmt.Sprintf("token endpoint unreachable (%s)", failureClass(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fe := &fetchError{msg: fmt.Sprintf("token endpoint returned status %d", resp.StatusCode)}
		if resp.StatusCode == http.StatusTooManyRequests {
			fe.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return "", 0, fe
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil || strings.TrimSpace(out.AccessToken) == "" || !strings.EqualFold(out.TokenType, "Bearer") {
		return "", 0, &fetchError{msg: "token endpoint returned no bearer access token"}
	}
	if out.ExpiresIn <= 0 {
		return "", 0, &fetchError{msg: "token endpoint returned no token lifetime"}
	}
	return out.AccessToken, time.Duration(min(out.ExpiresIn, int64(maxLifetime/time.Second))) * time.Second, nil
}

// parseRetryAfter reads a Retry-After given in seconds, capped at the backoff maximum. A number
// too large for int64 is the cap; anything else (a date, a negative, text) is no hint.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(strings.TrimSpace(v), "-"):
		return backoffMax
	case err != nil || secs <= 0:
		return 0
	}
	return time.Duration(min(secs, int64(backoffMax/time.Second))) * time.Second
}

// failureClass names why a request failed without quoting the error, which can carry the URL.
func failureClass(err error) string {
	var netErr net.Error
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.As(err, &cert):
		return "tls certificate"
	case errors.As(err, &dns):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "network"
	}
}
