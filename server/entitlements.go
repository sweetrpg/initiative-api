package server

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
)

// entitlementPrefix selects every Initiative key in the catalog.
const entitlementPrefix = "initiative."

// EntitlementItem is one key as the caller holds it. Value is a bool for flags and an integer
// for limits. Usage is the caller's count against a limit; it is omitted for flags and for limits
// whose resource this service does not store yet.
type EntitlementItem struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
	Usage *int64 `json:"usage,omitempty"`
}

type entitlementHandlers struct {
	svc   Services
	usage map[string]func(ctx context.Context, owner string) (int64, error)
}

func setupEntitlementHandlers(g *gin.RouterGroup, s Services) {
	h := entitlementHandlers{svc: s, usage: map[string]func(context.Context, string) (int64, error){
		"initiative.theme_slots":      s.Themes.Count,
		"initiative.saved_rosters":    s.Rosters.Count,
		"initiative.saved_encounters": s.Encounters.Count,
	}}
	g.GET("/entitlements", h.list)
}

// list shows the caller's Initiative entitlements with usage so clients can render state and
// upgrade prompts. Clients never use it to decide anything; enforcement stays in the handlers
// that create things.
func (h entitlementHandlers) list(c *gin.Context) {
	owner := authz.Viewer(c)
	results, err := h.svc.Entitlements.Prefix(c.Request.Context(), owner, authz.Token(c), entitlementPrefix)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	items := make([]EntitlementItem, 0, len(results))
	for _, r := range results {
		it := EntitlementItem{Key: r.Key, Type: r.Type}
		if r.Type == "flag" {
			it.Value = r.Flag
		} else {
			it.Value = r.Limit
		}
		if count, ok := h.usage[r.Key]; ok {
			n, err := count(c.Request.Context(), owner)
			if err != nil {
				writeError(c, err, nil)
				return
			}
			it.Usage = &n
		}
		items = append(items, it)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
