// Package entitlements is the client for entitlements-api's check routes
// (docs/entitlements-check-contract.md in the platform repo, PADR-0034).
//
// The caller's own bearer token is forwarded on every call (PADR-0037, no service tokens), so a
// check only answers for the user holding that token. Results are cached per (user, key) for the
// TTL the service returns. When the service cannot be reached, FailClosed callers (creates and
// expansions) get ErrUnavailable and FailOpen callers (reads and edits) get the last known value
// or a Degraded result they must not gate on.
package entitlements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	// maxTTL bounds a service-supplied TTL so a bad value cannot pin a stale entitlement.
	maxTTL = 5 * time.Minute
	// maxEntries bounds the cache; past it, expired entries go first, then everything.
	maxEntries  = 10000
	callTimeout = 3 * time.Second
	maxBody     = 64 << 10
)

// Policy says what a check does when entitlements-api cannot be reached.
type Policy int

const (
	// FailClosed returns ErrUnavailable. Use it for creates and expansions.
	FailClosed Policy = iota
	// FailOpen returns the last known value, or a Degraded result. Use it for reads and edits.
	FailOpen
)

var (
	// ErrUnavailable means entitlements-api could not be reached or answered with a server error.
	ErrUnavailable = errors.New("entitlements: service unavailable")
	// ErrUnauthorized means the service rejected the user's token; the caller returns 401.
	ErrUnauthorized = errors.New("entitlements: token rejected")
	// ErrUnknownKey means the key is not in the catalog: a programming error, never defaulted.
	ErrUnknownKey = errors.New("entitlements: key not registered")
)

// Result is one checked key. Flag is set for type "flag", Limit for type "limit".
type Result struct {
	Key   string
	Type  string
	Flag  bool
	Limit int
	// Degraded is true when no value is known because the service was down under FailOpen.
	// Flag and Limit are meaningless then; the caller skips gating.
	Degraded bool
}

type entry struct {
	res     Result
	expires time.Time
}

// Client calls entitlements-api. The zero value is not usable; use New.
type Client struct {
	base string
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// New returns a Client for the entitlements-api base URL.
func New(baseURL string) *Client {
	return &Client{
		base:  baseURL,
		http:  &http.Client{Timeout: callTimeout},
		now:   time.Now,
		cache: map[string]entry{},
	}
}

// Check returns the user's value for key. user is the verified token subject (the cache key);
// token is that user's bearer token, forwarded to the service.
func (c *Client) Check(ctx context.Context, user, token, key string, policy Policy) (Result, error) {
	ck := user + "\x00" + key
	if res, ok := c.fresh(ck); ok {
		return res, nil
	}

	res, ttl, err := c.fetch(ctx, token, key)
	switch {
	case err == nil:
		c.store(ck, res, ttl)
		return res, nil
	case errors.Is(err, ErrUnavailable) && policy == FailOpen:
		if res, ok := c.stale(ck); ok {
			return res, nil
		}
		return Result{Key: key, Degraded: true}, nil
	default:
		if errors.Is(err, ErrUnknownKey) {
			slog.ErrorContext(ctx, "entitlement key not registered", "key", key)
		}
		return Result{}, err
	}
}

func (c *Client) fetch(ctx context.Context, token, key string) (Result, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/check?key="+url.QueryEscape(key), nil)
	if err != nil {
		return Result{}, 0, fmt.Errorf("entitlements: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "entitlements call failed", "key", key, "error", err)
		return Result{}, 0, ErrUnavailable
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return decode(resp, key)
	case resp.StatusCode == http.StatusUnauthorized:
		return Result{}, 0, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return Result{}, 0, ErrUnknownKey
	default:
		slog.WarnContext(ctx, "entitlements call rejected", "key", key, "status", resp.StatusCode)
		return Result{}, 0, ErrUnavailable
	}
}

func decode(resp *http.Response, key string) (Result, time.Duration, error) {
	var body struct {
		Key   string `json:"key"`
		Type  string `json:"type"`
		Value any    `json:"value"`
		TTL   int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return Result{}, 0, ErrUnavailable
	}
	res := Result{Key: key, Type: body.Type}
	switch v := body.Value.(type) {
	case bool:
		res.Flag = v
	case float64:
		if v < 0 || v != float64(int(v)) {
			return Result{}, 0, ErrUnavailable
		}
		res.Limit = int(v)
	default:
		return Result{}, 0, ErrUnavailable
	}
	return res, min(time.Duration(body.TTL)*time.Second, maxTTL), nil
}

func (c *Client) fresh(ck string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[ck]
	if !ok || !c.now().Before(e.expires) {
		return Result{}, false
	}
	return e.res, true
}

func (c *Client) stale(ck string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[ck]
	return e.res, ok
}

func (c *Client) store(ck string, res Result, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxEntries {
		now := c.now()
		for k, e := range c.cache {
			if !now.Before(e.expires) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= maxEntries {
			c.cache = map[string]entry{}
		}
	}
	c.cache[ck] = entry{res: res, expires: c.now().Add(ttl)}
}
