package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
)

const (
	// tokenCacheKey holds the cached Auth0 access token (PADR-0041).
	tokenCacheKey = "reconciler:token"
	// tokenSkew is how long before expiry a token stops being served from the cache.
	tokenSkew = 120 * time.Second
)

// TokenSource supplies the bearer the reconciler sends to auth-api.
type TokenSource interface {
	// Token returns a usable access token.
	Token(ctx context.Context) (string, error)
	// Refresh discards the current token after auth-api rejected it and returns a new one.
	Refresh(ctx context.Context) (string, error)
}

// ErrNoRefresh is returned by a source that cannot replace a rejected token.
var ErrNoRefresh = errors.New("reconcile: token cannot be refreshed")

// StaticToken is a TokenSource for one fixed token.
type StaticToken string

// Token returns the fixed token.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// Refresh always fails: a fixed token has no replacement.
func (s StaticToken) Refresh(context.Context) (string, error) { return "", ErrNoRefresh }

// ClientCredentials fetches an Auth0 machine-to-machine access token with the client-credentials
// grant and caches it in Redis until 120 seconds before it expires, so a job that runs every five
// minutes uses about 30 Auth0 tokens a month instead of 8,640 (PADR-0041). The secret is never
// logged or put in an error.
type ClientCredentials struct {
	TokenURL, ClientID, ClientSecret, Audience string
	Pool                                       *redis.Pool
	HTTP                                       *http.Client

	token string // this run's token, so one run reads Redis once
	err   error  // sticky: one failed fetch fails the rest of the run without hitting Auth0 again
}

// NewClientCredentials returns a source that caches in pool.
func NewClientCredentials(tokenURL, clientID, clientSecret, audience string, pool *redis.Pool) *ClientCredentials {
	return &ClientCredentials{TokenURL: tokenURL, ClientID: clientID, ClientSecret: clientSecret, Audience: audience,
		Pool: pool, HTTP: &http.Client{Timeout: callTimeout}}
}

// Token returns the cached token, or fetches and caches a new one.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.token != "" {
		return c.token, nil
	}
	tok, err := c.cached()
	if err == nil && tok == "" {
		tok, err = c.fetchAndStore(ctx)
	}
	return c.remember(tok, err)
}

// Refresh clears the cached token and fetches a new one.
func (c *ClientCredentials) Refresh(ctx context.Context) (string, error) {
	c.token, c.err = "", nil
	conn := c.Pool.Get()
	defer func() { _ = conn.Close() }()
	if _, err := conn.Do("DEL", tokenCacheKey); err != nil {
		return c.remember("", fmt.Errorf("clear cached token: %w", err))
	}
	tok, err := c.fetchAndStore(ctx)
	return c.remember(tok, err)
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
	tok, lifetime, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
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
		return "", 0, errors.New("token endpoint unreachable") // the URL error text could echo the request
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil || strings.TrimSpace(out.AccessToken) == "" {
		return "", 0, errors.New("token endpoint returned no access token")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}
