package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// userChecker binds an entitlements client to one request's user and bearer token.
type userChecker struct {
	client      *entitlements.Client
	user, token string
	// record holds the user's last first-party entitlement values. A first-party check writes
	// them; an integration credential, which has no user bearer to call entitlements-api with
	// (PADR-0034, PADR-0037), reads them instead (viaCredential).
	record        *data.OwnerEntitlements
	viaCredential bool
}

func (u userChecker) Check(ctx context.Context, key string, p entitlements.Policy) (entitlements.Result, error) {
	if u.viaCredential {
		return u.recorded(ctx, key)
	}
	res, err := u.client.Check(ctx, u.user, u.token, key, p)
	if err == nil && u.record != nil && !res.Stale && !res.Degraded {
		u.remember(ctx, res)
	}
	return res, err
}

// recorded answers from the owner's record. With no record of the key it fails closed with
// ErrLimitsNotSynced: the owner has to use the app once with their own sign-in.
func (u userChecker) recorded(ctx context.Context, key string) (entitlements.Result, error) {
	if u.record == nil {
		return entitlements.Result{}, domain.ErrLimitsNotSynced
	}
	e, _, err := u.record.Get(ctx, u.user, key)
	if errors.Is(err, data.ErrNotFound) {
		return entitlements.Result{}, domain.ErrLimitsNotSynced
	}
	if err != nil {
		return entitlements.Result{}, err
	}
	return entitlements.Result{Key: e.Key, Type: e.Type, Flag: e.Flag, Limit: e.Limit}, nil
}

// remember records a fresh first-party result. It is best effort: a failed write keeps the old
// record and is logged, and the request goes on.
func (u userChecker) remember(ctx context.Context, results ...entitlements.Result) {
	entries := make([]data.OwnerEntitlement, 0, len(results))
	for _, r := range results {
		entries = append(entries, data.OwnerEntitlement{Key: r.Key, Type: r.Type, Limit: r.Limit, Flag: r.Flag})
	}
	if err := u.record.Record(ctx, u.user, entries...); err != nil {
		slog.WarnContext(ctx, "recording owner entitlements failed", "error", err)
	}
}

const ownerRecordKey = "initiative.ownerEntitlements"

// withOwnerRecord puts the owner-entitlement record on the request, for checkerFor.
func withOwnerRecord(rec *data.OwnerEntitlements) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rec != nil {
			c.Set(ownerRecordKey, rec)
		}
		c.Next()
	}
}

func checkerFor(c *gin.Context, client *entitlements.Client) domain.Checker {
	rec, _ := c.Get(ownerRecordKey)
	r, _ := rec.(*data.OwnerEntitlements)
	_, viaCredential := c.Get(credentialContextKey)
	return userChecker{client: client, user: authz.Viewer(c), token: authz.Token(c), record: r, viaCredential: viaCredential}
}

type themeBody struct {
	Name              string `json:"name"`
	Style             string `json:"style"`
	MainColor         string `json:"main_color"`
	Accent1Color      string `json:"accent1_color"`
	Accent2Color      string `json:"accent2_color"`
	Background1Color  string `json:"background1_color"`
	Background2Color  string `json:"background2_color"`
	PrimaryFontName   string `json:"primary_font_name"`
	SecondaryFontName string `json:"secondary_font_name"`
	BackgroundImage   string `json:"background_image"`
}

type themePatchBody struct {
	Revision          int64   `json:"revision"`
	Name              *string `json:"name"`
	Style             *string `json:"style"`
	MainColor         *string `json:"main_color"`
	Accent1Color      *string `json:"accent1_color"`
	Accent2Color      *string `json:"accent2_color"`
	Background1Color  *string `json:"background1_color"`
	Background2Color  *string `json:"background2_color"`
	PrimaryFontName   *string `json:"primary_font_name"`
	SecondaryFontName *string `json:"secondary_font_name"`
	BackgroundImage   *string `json:"background_image"`
}

type themeHandlers struct {
	svc *domain.Themes
	ent *entitlements.Client
}

func setupThemeHandlers(g *gin.RouterGroup, svc *domain.Themes, ent *entitlements.Client) {
	h := themeHandlers{svc: svc, ent: ent}
	g.GET("/themes", h.list)
	g.GET("/themes/packaged", h.packaged)
	g.GET("/themes/:id", h.get)
	g.POST("/themes", h.create)
	g.PATCH("/themes/:id", h.update)
	g.DELETE("/themes/:id", h.remove)
}

func (h themeHandlers) list(c *gin.Context) {
	q := data.Query{Cursor: c.Query("cursor")}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(c, "limit must be a positive integer")
			return
		}
		q.Limit = n
	}
	page, err := h.svc.List(c.Request.Context(), authz.Viewer(c), q)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": page.Items, "next_cursor": page.NextCursor})
}

func (h themeHandlers) packaged(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": h.svc.Packaged()})
}

func (h themeHandlers) get(c *gin.Context) {
	t, err := h.svc.Get(c.Request.Context(), authz.Viewer(c), c.Param("id"))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, t)
}

func (h themeHandlers) create(c *gin.Context) {
	var b themeBody
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, "request body must be a JSON theme")
		return
	}
	owner := authz.Viewer(c)
	t, err := h.svc.Create(c.Request.Context(), owner, owner, domain.ThemeInput{
		Name: b.Name, Style: b.Style, MainColor: b.MainColor, Accent1Color: b.Accent1Color,
		Accent2Color: b.Accent2Color, Background1Color: b.Background1Color, Background2Color: b.Background2Color,
		PrimaryFontName: b.PrimaryFontName, SecondaryFontName: b.SecondaryFontName, BackgroundImage: b.BackgroundImage,
	}, checkerFor(c, h.ent))
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.JSON(http.StatusCreated, t)
}

func (h themeHandlers) update(c *gin.Context) {
	var b themePatchBody
	if err := c.ShouldBindJSON(&b); err != nil || b.Revision < 1 {
		badRequest(c, "request body must be JSON with the theme's current revision")
		return
	}
	owner := authz.Viewer(c)
	t, err := h.svc.Update(c.Request.Context(), owner, owner, c.Param("id"), b.Revision, domain.ThemePatch{
		Name: b.Name, Style: b.Style, MainColor: b.MainColor, Accent1Color: b.Accent1Color,
		Accent2Color: b.Accent2Color, Background1Color: b.Background1Color, Background2Color: b.Background2Color,
		PrimaryFontName: b.PrimaryFontName, SecondaryFontName: b.SecondaryFontName, BackgroundImage: b.BackgroundImage,
	})
	if err != nil {
		writeError(c, err, t)
		return
	}
	c.JSON(http.StatusOK, t)
}

func (h themeHandlers) remove(c *gin.Context) {
	rev, err := strconv.ParseInt(c.Query("revision"), 10, 64)
	if err != nil || rev < 1 {
		badRequest(c, "revision query parameter is required")
		return
	}
	owner := authz.Viewer(c)
	if err := h.svc.Delete(c.Request.Context(), owner, owner, c.Param("id"), rev); err != nil {
		writeError(c, err, nil)
		return
	}
	c.Status(http.StatusNoContent)
}
