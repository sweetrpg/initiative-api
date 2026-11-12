package server

import (
	"context"
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
}

func (u userChecker) Check(ctx context.Context, key string, p entitlements.Policy) (entitlements.Result, error) {
	return u.client.Check(ctx, u.user, u.token, key, p)
}

func checkerFor(c *gin.Context, client *entitlements.Client) domain.Checker {
	return userChecker{client: client, user: authz.Viewer(c), token: authz.Token(c)}
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
