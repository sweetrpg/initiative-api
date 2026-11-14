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
	// ErrDenied means auth-api refused the change: the caller lacks the relation, or is denied
	// the service. Callers answer 403.
	ErrDenied = errors.New("relations: not permitted")
	// ErrLimited means auth-api's mint or tuple limits stopped the call. Callers answer 429.
	ErrLimited = errors.New("relations: rate limited")
	// ErrConflict means auth-api refused the change as conflicting (the session's tuple cap).
	// Callers answer 409.
	ErrConflict = errors.New("relations: conflict")
	// ErrRejected means auth-api answered 400: this service sent a malformed request, so retrying
	// the same call never helps. Callers answer 500.
	ErrRejected = errors.New("relations: request rejected as malformed")
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

// Tuple is one membership relation, written for the session object the call names.
type Tuple struct {
	User     string `json:"user"` // "user:<canonical id>"
	Relation string `json:"relation"`
}

// Mint asks auth-api for a new session ID and writes the caller's gm tuple on it. The ID comes
// from auth-api, never from the client (PADR-0040).
func (c *Client) Mint(ctx context.Context, token string) (string, error) {
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/authz/sessions", token, nil, http.StatusCreated, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", ErrUnavailable
	}
	return out.SessionID, nil
}

// EndSession deletes every tuple on the session and closes its mint. Only the GM may.
func (c *Client) EndSession(ctx context.Context, token, sessionID string) error {
	return c.call(ctx, http.MethodDelete, "/authz/sessions/"+sessionID, token, nil, http.StatusNoContent, nil)
}

// WriteTuples applies membership tuple changes on one session in a single atomic write. Writing a
// tuple that exists and deleting one that does not are no-ops, so a retry is safe.
func (c *Client) WriteTuples(ctx context.Context, token, sessionID string, writes, deletes []Tuple) error {
	body := struct {
		SessionID string  `json:"session_id"`
		Writes    []Tuple `json:"writes"`
		Deletes   []Tuple `json:"deletes"`
	}{sessionID, writes, deletes}
	if body.Writes == nil {
		body.Writes = []Tuple{}
	}
	if body.Deletes == nil {
		body.Deletes = []Tuple{}
	}
	return c.call(ctx, http.MethodPost, "/authz/session-tuples", token, body, http.StatusOK, nil)
}

// call sends one authenticated request and maps the answer: 401 is ErrUnauthorized, 403 ErrDenied,
// 429 ErrLimited, anything else but want (or an unreadable body) ErrUnavailable.
func (c *Client) call(ctx context.Context, method, path, token string, body any, want int, out any) error {
	if token == "" {
		return ErrUnauthorized
	}
	var rdr io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("relations: encode request: %w", err)
		}
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("relations: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "auth-api call failed", "path", path, "error", err)
		return ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case want:
		if out == nil {
			return nil
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return ErrUnavailable
		}
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrDenied
	case http.StatusTooManyRequests:
		return ErrLimited
	case http.StatusConflict:
		return ErrConflict
	case http.StatusBadRequest:
		slog.ErrorContext(ctx, "auth-api rejected a session call as malformed", "path", path)
		return ErrRejected
	}
	slog.WarnContext(ctx, "auth-api rejected a session call", "path", path, "status", resp.StatusCode)
	return ErrUnavailable
}
