// Package entitlements is the client for entitlements-api's check routes
// (docs/entitlements-check-contract.md in the platform repo, PADR-0034).
//
// The caller's own bearer token is forwarded on every call (PADR-0037, no service tokens), so a
// check only answers for the user holding that token. Results are cached per (user, key) for the
// TTL the service returns. When the service cannot be reached, FailClosed callers (creates and
// expansions) get ErrUnavailable and FailOpen callers (reads and edits) get the last known value
// (at most maxStale past its TTL) or a Degraded result they must not gate on.
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
	maxEntries = 10000
	// maxStale bounds how long FailOpen may serve an expired value during an outage; past it the
	// result is Degraded and the caller skips gating.
	maxStale    = time.Hour
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
	var body checkBody
	if err := c.get(ctx, token, "/check?key="+url.QueryEscape(key), &body); err != nil {
		return Result{}, 0, err
	}
	return body.result(key)
}

// Prefix returns the user's values for every key under prefix in one call and caches each. It
// has no stale fallback: any failure is returned, for callers that only display the values.
func (c *Client) Prefix(ctx context.Context, user, token, prefix string) ([]Result, error) {
	var body struct {
		Results []checkBody `json:"results"`
	}
	if err := c.get(ctx, token, "/check/batch?prefix="+url.QueryEscape(prefix), &body); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(body.Results))
	for _, b := range body.Results {
		res, ttl, err := b.result(b.Key)
		if err != nil {
			return nil, err
		}
		c.store(user+"\x00"+b.Key, res, ttl)
		out = append(out, res)
	}
	return out, nil
}

// get calls path with the user's token and decodes a 200 body into out.
func (c *Client) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("entitlements: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "entitlements call failed", "path", path, "error", err)
		return ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return ErrUnavailable
		}
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrUnknownKey
	default:
		slog.WarnContext(ctx, "entitlements call rejected", "path", path, "status", resp.StatusCode)
		return ErrUnavailable
	}
}

type checkBody struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
	TTL   int    `json:"ttl_seconds"`
}

func (b checkBody) result(key string) (Result, time.Duration, error) {
	res := Result{Key: key, Type: b.Type}
	switch v := b.Value.(type) {
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
	return res, min(time.Duration(b.TTL)*time.Second, maxTTL), nil
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
	return e.res, ok && c.now().Before(e.expires.Add(maxStale))
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
