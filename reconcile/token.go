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
	if err := c.takeFetchGuard(); err != nil {
		return "", err
	}
	tok, lifetime, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.fetched = true
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
		return "", 0, fmt.Errorf("token endpoint unreachable (%s)", failureClass(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil || strings.TrimSpace(out.AccessToken) == "" || !strings.EqualFold(out.TokenType, "Bearer") {
		return "", 0, errors.New("token endpoint returned no bearer access token")
	}
	if out.ExpiresIn <= 0 {
		return "", 0, errors.New("token endpoint returned no token lifetime")
	}
	return out.AccessToken, time.Duration(min(out.ExpiresIn, int64(maxLifetime/time.Second))) * time.Second, nil
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
