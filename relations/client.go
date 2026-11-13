// Package relations is the client for auth-api's POST /authz/check-relation (PADR-0035).
//
// The caller's own bearer token is forwarded on every call (PADR-0037, no service tokens), so a
// check only answers for the user holding that token. Only positive decisions are cached, for a
// few seconds, so a removal takes effect within positiveTTL and a new grant at once. Any failure
// to get a decision is an error: session data fails closed (503), never open.
package relations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// positiveTTL bounds how long a removal can go unnoticed on a cached allow.
	positiveTTL = 5 * time.Second
	// maxEntries bounds the cache; past it, expired entries go first, then everything.
	maxEntries  = 10000
	callTimeout = 3 * time.Second
	maxBody     = 4 << 10
)

var (
	// ErrUnavailable means auth-api or OpenFGA could not give a decision. Callers answer 503.
	ErrUnavailable = errors.New("relations: authorization unavailable")
	// ErrUnauthorized means auth-api rejected the user's token. Callers answer 401.
	ErrUnauthorized = errors.New("relations: token rejected")
)

// Client calls auth-api. The zero value is not usable; use New.
type Client struct {
	base string
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]time.Time // key -> expiry of a cached allow
}

// New returns a Client for the auth-api base URL.
func New(baseURL string) *Client {
	return &Client{base: baseURL, http: &http.Client{Timeout: callTimeout}, now: time.Now, cache: map[string]time.Time{}}
}

// Check reports whether the holder of token has relation on object (for example
// "session:abc", "can_view"). user is the verified caller ID and only keys the cache; auth-api
// decides from the token. A deny is (false, nil).
func (c *Client) Check(ctx context.Context, user, token, object, relation string) (bool, error) {
	if user == "" || token == "" || object == "" || relation == "" {
		return false, fmt.Errorf("relations: user, token, object and relation are required")
	}
	key := user + "\x00" + object + "\x00" + relation
	if c.allowed(key) {
		return true, nil
	}
	ok, err := c.fetch(ctx, token, object, relation)
	if err != nil {
		return false, err
	}
	if ok {
		c.remember(key)
	}
	return ok, nil
}

func (c *Client) fetch(ctx context.Context, token, object, relation string) (bool, error) {
	payload, err := json.Marshal(map[string]string{"object": object, "relation": relation})
	if err != nil {
		return false, fmt.Errorf("relations: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/authz/check-relation", bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("relations: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "check-relation call failed", "error", err)
		return false, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Allowed bool `json:"allowed"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
			return false, ErrUnavailable
		}
		return body.Allowed, nil
	case http.StatusUnauthorized:
		return false, ErrUnauthorized
	default:
		slog.WarnContext(ctx, "check-relation rejected", "status", resp.StatusCode)
		return false, ErrUnavailable
	}
}

func (c *Client) allowed(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.cache[key]
	return ok && c.now().Before(exp)
}

func (c *Client) remember(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxEntries {
		now := c.now()
		for k, exp := range c.cache {
			if !now.Before(exp) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= maxEntries {
			c.cache = map[string]time.Time{}
		}
	}
	c.cache[key] = c.now().Add(positiveTTL)
}
