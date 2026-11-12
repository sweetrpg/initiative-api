package server

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

const (
	credentialContextKey = "initiative.credential"
	// Keys authz-client.go uses for the caller's identity; authz.Viewer and authz.Roles read them,
	// so a credential principal looks like its owner to every handler. TestCredentialPrincipal
	// fails if the library changes them.
	authzViewerKey = "authz.viewer"
	authzRolesKey  = "authz.roles"
)

// authenticate lets an integration secret ("ic_..." bearer) stand in for the user token. A secret
// resolves to its owner on every request, so a revoke applies to the next one. The principal gets
// the user role only and its scopes gate the method: read for GET/HEAD/OPTIONS, write otherwise.
// Anything else goes to userAuth (auth-api) unchanged.
func authenticate(creds *domain.Credentials, userAuth gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, _ := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if creds == nil || !strings.HasPrefix(secret, domain.SecretPrefix) {
			userAuth(c)
			return
		}
		cred, err := creds.Authenticate(c.Request.Context(), secret)
		if errors.Is(err, data.ErrNotFound) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorBody{Code: "unauthorized", Message: "Missing or invalid credential.", CorrelationID: CorrelationID(c)})
			return
		}
		if err != nil {
			writeError(c, err, nil)
			return
		}
		need := data.ScopeWrite
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			need = data.ScopeRead
		}
		if !slices.Contains(cred.Scopes, need) {
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorBody{Code: "scope_missing", Message: "This credential lacks the " + need + " scope.", CorrelationID: CorrelationID(c)})
			return
		}
		c.Set(authzViewerKey, cred.OwnerUserID)
		c.Set(authzRolesKey, []string{authz.RoleUser})
		c.Set(credentialContextKey, cred.ID)
		c.Next()
	}
}

// userOnly rejects integration credentials: account-level routes need the user's own bearer
// (they forward it to entitlements-api, PADR-0037), and a credential must not manage credentials.
func userOnly(c *gin.Context) {
	if _, ok := c.Get(credentialContextKey); ok {
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorBody{Code: "user_token_required", Message: "This request needs your own sign-in, not an integration credential.", CorrelationID: CorrelationID(c)})
		return
	}
	c.Next()
}

type credentialHandlers struct {
	svc Services
}

func setupCredentialHandlers(g *gin.RouterGroup, s Services) {
	h := credentialHandlers{s}
	g.POST("/credentials", h.create)
	g.GET("/credentials", h.list)
	g.POST("/credentials/:id/revoke", h.revoke)
}

type credentialBody struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

// create returns the secret once, alongside the stored record.
func (h credentialHandlers) create(c *gin.Context) {
	var b credentialBody
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be JSON with label and scopes")
		return
	}
	cred, secret, err := h.svc.Credentials.Create(c.Request.Context(), authz.Viewer(c), b.Label, b.Scopes, checkerFor(c, h.svc.Entitlements))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"credential": cred, "secret": secret})
}

func (h credentialHandlers) list(c *gin.Context) {
	page, err := h.svc.Credentials.List(c.Request.Context(), authz.Viewer(c), data.Query{Cursor: c.Query("cursor")})
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": page.Items, "next_cursor": page.NextCursor})
}

func (h credentialHandlers) revoke(c *gin.Context) {
	cred, err := h.svc.Credentials.Revoke(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, cred)
}

// setupAdminCredentialHandlers registers the administrator's reopen. The group takes no integration
// credentials, so only an admin's own bearer reaches it.
func setupAdminCredentialHandlers(g *gin.RouterGroup, creds *domain.Credentials) {
	g.POST("/admin/credentials/reopen", func(c *gin.Context) {
		actor := authz.Viewer(c)
		if err := creds.Reopen(c.Request.Context(), actor); err != nil {
			writeError(c, err, nil)
			return
		}
		slog.WarnContext(c.Request.Context(), "audit: integration credentials reopened", "audit", true, "actor", actor, "correlation_id", CorrelationID(c))
		c.JSON(http.StatusOK, gin.H{"open": true})
	})
}
