package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

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
}

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

// Create issues a credential for owner and returns it with its secret, which is never available
// again. At or over the entitled limit of active credentials it returns *EntitlementError.
func (s *Credentials) Create(ctx context.Context, owner, label string, scopes []string, chk Checker) (data.IntegrationCredential, string, error) {
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
	return s.repo.Authenticate(ctx, hashSecret(secret))
}
