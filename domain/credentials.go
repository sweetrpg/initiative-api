package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store"
)

// KeyIntegrationCredentials is the entitlement limit on active integration credentials.
const KeyIntegrationCredentials = "initiative.integration_credentials"

// SecretPrefix starts every integration secret, so the auth middleware can tell one from a user
// token without a lookup.
const SecretPrefix = "ic_"

const maxLabel = 100

// Credentials issues, lists, revokes, and authenticates integration credentials.
type Credentials struct {
	repo *data.Credentials
	open atomic.Bool
}

// ErrCredentialsClosed means integration credentials are off: the auth-event consumer is not
// caught up, or it fell behind the stream's retention and awaits a reconcile (PADR-0039). It is
// fail closed, so no credential authenticates and none is issued.
var ErrCredentialsClosed = errors.New("integration credentials are closed")

// NewCredentials builds the service on db.
func NewCredentials(db store.Database) *Credentials {
	return &Credentials{repo: data.NewCredentials(db)}
}

// hashSecret is the stored form of a secret. The secret is 256 random bits, so a plain SHA-256 is
// enough; a slow hash would only add cost to every request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: at least one scope is required", data.ErrInvalid)
	}
	for _, s := range scopes {
		if s != data.ScopeRead && s != data.ScopeWrite {
			return fmt.Errorf("%w: unknown scope %q", data.ErrInvalid, s)
		}
	}
	return nil
}

// Open lets credentials work, unless a persisted close from an earlier run is still in force.
// Call it once the auth-event consumer has caught up. It reports whether the gate is now open.
func (s *Credentials) Open(ctx context.Context) (bool, error) {
	closed, err := s.repo.GateClosed(ctx)
	if err != nil || closed {
		return false, err
	}
	s.open.Store(true)
	return true, nil
}

// Close turns credentials off and persists that, so a restart stays closed until Reopen.
func (s *Credentials) Close(ctx context.Context) error {
	s.open.Store(false)
	return s.repo.SetGate(ctx, true)
}

// Reopen clears a persisted close after an administrator's reconcile.
func (s *Credentials) Reopen(ctx context.Context) error {
	if err := s.repo.SetGate(ctx, false); err != nil {
		return err
	}
	s.open.Store(true)
	return nil
}

// RevokeOwner ends every active credential of an owner the auth service has denied or stripped of
// a role, and returns how many it ended. Credentials stay for audit.
func (s *Credentials) RevokeOwner(ctx context.Context, owner, reason string) (int, error) {
	if reason != data.ReasonOwnerDenied && reason != data.ReasonOwnerRoleRemoved {
		return 0, fmt.Errorf("%w: unknown revoke reason %q", data.ErrInvalid, reason)
	}
	return s.repo.RevokeAllByOwner(ctx, owner, reason)
}

// Create issues a credential for owner and returns it with its secret, which is never available
// again. At or over the entitled limit of active credentials it returns *EntitlementError.
func (s *Credentials) Create(ctx context.Context, owner, label string, scopes []string, chk Checker) (data.IntegrationCredential, string, error) {
	if !s.open.Load() {
		return data.IntegrationCredential{}, "", ErrCredentialsClosed
	}
	label = strings.TrimSpace(label)
	if label == "" || len([]rune(label)) > maxLabel {
		return data.IntegrationCredential{}, "", fmt.Errorf("%w: label is required and at most %d characters", data.ErrInvalid, maxLabel)
	}
	if err := validateScopes(scopes); err != nil {
		return data.IntegrationCredential{}, "", err
	}
	res, err := chk.Check(ctx, KeyIntegrationCredentials, entitlements.FailClosed)
	if err != nil {
		return data.IntegrationCredential{}, "", err
	}
	// ponytail: count-then-insert can overshoot the limit by the number of concurrent creates.
	// Tighten with a per-owner slot index if that ever matters.
	n, err := s.repo.CountActive(ctx, owner)
	if err != nil {
		return data.IntegrationCredential{}, "", err
	}
	if n >= int64(res.Limit) {
		return data.IntegrationCredential{}, "", &EntitlementError{Key: KeyIntegrationCredentials, Limit: res.Limit}
	}
	secret, err := newSecret()
	if err != nil {
		return data.IntegrationCredential{}, "", err
	}
	c := data.IntegrationCredential{Label: label, Scopes: slices.Compact(slices.Sorted(slices.Values(scopes))), SecretHash: hashSecret(secret)}
	if err := s.repo.Create(ctx, owner, owner, &c); err != nil {
		return data.IntegrationCredential{}, "", err
	}
	return c, secret, nil
}

// List returns owner's credentials, newest first. Revoked ones stay listed.
func (s *Credentials) List(ctx context.Context, owner string, q data.Query) (data.Page[data.IntegrationCredential], error) {
	return s.repo.List(ctx, owner, q)
}

// Revoke ends owner's credential at once; repeating it is harmless.
func (s *Credentials) Revoke(ctx context.Context, owner, id string) (data.IntegrationCredential, error) {
	return s.repo.Revoke(ctx, owner, owner, id)
}

// Authenticate resolves a presented secret to its credential and records the use. A revoked,
// deleted, or unknown secret is data.ErrNotFound; the next request after a revoke fails here.
func (s *Credentials) Authenticate(ctx context.Context, secret string) (data.IntegrationCredential, error) {
	if !strings.HasPrefix(secret, SecretPrefix) {
		return data.IntegrationCredential{}, data.ErrNotFound
	}
	if !s.open.Load() {
		return data.IntegrationCredential{}, ErrCredentialsClosed
	}
	return s.repo.Authenticate(ctx, hashSecret(secret))
}
