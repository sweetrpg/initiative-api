package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	readPage    = 100
	callTimeout = 10 * time.Second
	maxBody     = 1 << 20
)

// AuthAPI calls auth-api's internal tuple endpoints with the reconciler's own bearer
// (PADR-0037, PADR-0038). No service token is involved.
type AuthAPI struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewAuthAPI returns a client for base using token.
func NewAuthAPI(base, token string) *AuthAPI {
	return &AuthAPI{Base: base, Token: token, HTTP: &http.Client{Timeout: callTimeout}}
}

// Read returns every tuple on object, following continuation tokens.
func (a *AuthAPI) Read(ctx context.Context, object string) ([]Tuple, error) {
	var all []Tuple
	token := ""
	for {
		q := url.Values{"object": {object}, "page_size": {fmt.Sprint(readPage)}}
		if token != "" {
			q.Set("continuation_token", token)
		}
		var page struct {
			Tuples []Tuple `json:"tuples"`
			Next   string  `json:"continuation_token"`
		}
		if err := a.do(ctx, http.MethodGet, "/internal/openfga/tuples?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tuples...)
		if page.Next == "" {
			return all, nil
		}
		token = page.Next
	}
}

// Write applies one batch (at most 100 tuples in total) atomically.
func (a *AuthAPI) Write(ctx context.Context, writes, deletes []Tuple) error {
	body, err := json.Marshal(map[string][]Tuple{"writes": writes, "deletes": deletes})
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodPost, "/internal/openfga/tuples", body, nil)
}

func (a *AuthAPI) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("auth-api %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrTokenRejected
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent:
		return fmt.Errorf("auth-api %s: status %d", path, resp.StatusCode)
	case out == nil:
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
}

// ListMints returns one page of open mints at least minAge old (by auth-api's clock) after the
// cursor, and the cursor for the next page, empty on the last.
func (a *AuthAPI) ListMints(ctx context.Context, minAge time.Duration, after string) ([]Mint, string, error) {
	q := url.Values{"min_age": {minAge.String()}, "limit": {"200"}}
	if after != "" {
		q.Set("after", after)
	}
	var page struct {
		Mints []Mint `json:"mints"`
		Next  string `json:"next"`
	}
	if err := a.do(ctx, http.MethodGet, "/internal/openfga/session-mints?"+q.Encode(), nil, &page); err != nil {
		return nil, "", err
	}
	return page.Mints, page.Next, nil
}

// CloseMint deletes every tuple on the minted session and closes its mint.
func (a *AuthAPI) CloseMint(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodPost, "/internal/openfga/session-mints/"+url.PathEscape(id)+"/close", nil, nil)
}
