package server

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/sweetrpg/api-core.go/ratelimit"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

// resolvedCredentialKey holds the credential the limiter step already looked up, so authenticate
// does not query it twice.
const resolvedCredentialKey = "ratelimit.credential"

// RateLimit returns the per-client limiter (PADR-0036) preceded by a step that sets its client key.
// The shared limiter keys on X-API-Key, else the IP, and both are caller-controlled, so that step
// never lets the caller's header through:
//   - a bearer that resolves to a live integration credential is keyed by the credential's ID,
//     so credentials are distinct clients;
//   - everything else, including a made-up "ic_" secret, is keyed by IP.
func RateLimit(pool *redis.Pool, opts ratelimit.Options, creds *domain.Credentials) []gin.HandlerFunc {
	return []gin.HandlerFunc{typedErrors(), clientKey(creds), ratelimit.Middleware(pool, opts)}
}

func clientKey(creds *domain.Credentials) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Header.Del("X-API-Key")
		secret, _ := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if creds != nil && strings.HasPrefix(secret, domain.SecretPrefix) {
			// Any failure (unknown, revoked, store error) leaves the IP key; authenticate then
			// rejects or reports it.
			if cred, err := creds.Authenticate(c.Request.Context(), secret); err == nil {
				c.Set(resolvedCredentialKey, cred)
				c.Request.Header.Set("X-API-Key", "cred:"+cred.ID)
			}
		}
		c.Next()
	}
}

func resolvedCredential(c *gin.Context) (data.IntegrationCredential, bool) {
	v, ok := c.Get(resolvedCredentialKey)
	if !ok {
		return data.IntegrationCredential{}, false
	}
	cred, ok := v.(data.IntegrationCredential)
	return cred, ok
}
