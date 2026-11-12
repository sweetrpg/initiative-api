package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
)

func TestCredentialSecretResolvesToItsOwnerUntilRevoked(t *testing.T) {
	s, _ := newCreds(t)
	c, secret, err := s.Create(ctx, "u1", "foundry", []string{"read"}, stubChecker{limit: 2})
	if err != nil || !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("create = %v, %q", err, secret)
	}

	got, err := s.Authenticate(ctx, secret)
	if err != nil || got.ID != c.ID || got.OwnerUserID != "u1" {
		t.Fatalf("authenticate = %+v, %v", got, err)
	}
	if _, err := s.Revoke(ctx, "u1", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("revoked = %v, want ErrNotFound", err)
	}
	if _, err := s.Authenticate(ctx, "not-a-secret"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("no prefix = %v, want ErrNotFound", err)
	}
}

func TestCredentialCreateValidatesAndHonorsTheLimit(t *testing.T) {
	s, _ := newCreds(t)
	tests := []struct {
		name   string
		label  string
		scopes []string
		chk    Checker
	}{
		{"blank label", " ", []string{"read"}, stubChecker{limit: 2}},
		{"no scopes", "x", nil, stubChecker{limit: 2}},
		{"unknown scope", "x", []string{"admin"}, stubChecker{limit: 2}},
		{"limit zero", "x", []string{"read"}, stubChecker{limit: 0}},
		{"entitlements down fails closed", "x", []string{"read"}, stubChecker{err: errors.New("down")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.Create(ctx, "u1", tt.label, tt.scopes, tt.chk); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if page, _ := s.List(ctx, "u1", data.Query{}); len(page.Items) != 0 {
		t.Fatalf("rejected creates stored %d credentials", len(page.Items))
	}
}

func TestRevokeOwnerEndsCredentialsAndRejectsUnknownReasons(t *testing.T) {
	s, _ := newCreds(t)
	_, secret, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 2})

	n, err := s.RevokeOwner(ctx, "u1", data.ReasonOwnerDenied)

	if err != nil || n != 1 {
		t.Fatalf("revoked %d, %v", n, err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("after owner revoke = %v", err)
	}
	if _, err := s.RevokeOwner(ctx, "u1", "because"); !errors.Is(err, data.ErrInvalid) {
		t.Fatalf("unknown reason = %v, want ErrInvalid", err)
	}
}

func TestClosedCredentialsFailClosedUntilReopened(t *testing.T) {
	s, _ := newCreds(t)
	_, secret, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 2})
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrCredentialsClosed) {
		t.Fatalf("closed = %v, want ErrCredentialsClosed", err)
	}

	if err := s.Reopen(ctx, "admin"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Authenticate(ctx, secret); err != nil {
		t.Fatalf("reopened = %v", err)
	}
}
