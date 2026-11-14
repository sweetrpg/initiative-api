package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthAPIReadsAllPagesWithTheBearer(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		next := ""
		if r.URL.Query().Get("continuation_token") == "" {
			next = "p2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tuples": []Tuple{tup("s1", r.URL.Query().Get("continuation_token")+"x", "gm")}, "continuation_token": next})
	}))
	defer srv.Close()
	got, err := NewAuthAPI(srv.URL, "tok").Read(context.Background(), "session:s1")
	if err != nil || len(got) != 2 || auth != "Bearer tok" {
		t.Fatalf("got=%v err=%v auth=%q", got, err, auth)
	}
}

func TestAuthAPIMapsAuthFailuresToTokenRejected(t *testing.T) {
	for _, code := range []int{401, 403} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		err := NewAuthAPI(srv.URL, "tok").Write(context.Background(), []Tuple{tup("s1", "u", "gm")}, nil)
		srv.Close()
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("status %d: err = %v", code, err)
		}
	}
}

func TestAuthAPIOtherErrorsAreNotTokenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	err := NewAuthAPI(srv.URL, "tok").Write(context.Background(), []Tuple{tup("s1", "u", "gm")}, nil)
	if err == nil || errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestAuthAPIListsMintsWithTheCursorAndAgeAndClosesOne(t *testing.T) {
	var gotQuery, gotPath, gotMethod, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotPath, gotMethod, auth = r.URL.RawQuery, r.URL.Path, r.Method, r.Header.Get("Authorization")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"mints": []Mint{{ID: "0123456789abcdef01234567"}}, "next": "cur2"})
	}))
	defer srv.Close()
	api := NewAuthAPI(srv.URL, "tok")

	mints, next, err := api.ListMints(context.Background(), 15*time.Minute, "cur1")

	if err != nil || len(mints) != 1 || next != "cur2" || auth != "Bearer tok" {
		t.Fatalf("list = %v, %q, %v, auth %q", mints, next, err, auth)
	}
	for _, want := range []string{"min_age=15m0s", "after=cur1", "limit=200"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q lacks %s", gotQuery, want)
		}
	}
	if err := api.CloseMint(context.Background(), "0123456789abcdef01234567"); err != nil {
		t.Fatalf("close = %v, want a 204 to count as success", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/internal/openfga/session-mints/0123456789abcdef01234567/close" {
		t.Errorf("close request = %s %s", gotMethod, gotPath)
	}
}

func TestAuthAPIMintCallsMapAuthFailuresToTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	api := NewAuthAPI(srv.URL, "tok")
	if _, _, err := api.ListMints(context.Background(), time.Minute, ""); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("list = %v, want ErrTokenRejected", err)
	}
	if err := api.CloseMint(context.Background(), "x"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("close = %v, want ErrTokenRejected", err)
	}
}
