package server

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/sweetrpg/api-core.go/ratelimit"
	"github.com/sweetrpg/initiative-api/domain"
)

// RateLimit returns the per-client limiter (PADR-0036) preceded by a step that makes each
// integration credential its own client. The shared limiter keys on X-API-Key, else the IP, so an
// "ic_" bearer is mapped onto that key from a hash of its secret: two credentials, and a credential
// and its owner's browser, never share a budget. A value sent by the caller is overwritten.
func RateLimit(pool *redis.Pool, opts ratelimit.Options) []gin.HandlerFunc {
	return []gin.HandlerFunc{credentialClient, ratelimit.Middleware(pool, opts)}
}

func credentialClient(c *gin.Context) {
	secret, _ := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if strings.HasPrefix(secret, domain.SecretPrefix) {
		sum := sha256.Sum256([]byte(secret))
		c.Request.Header.Set("X-API-Key", "cred:"+hex.EncodeToString(sum[:8]))
	}
	c.Next()
}
