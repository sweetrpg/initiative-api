package server

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// revisionOf reads the revision a write is based on: If-Match (`"r3"` or `r3`, the form the
// encounter ETag takes), else the `revision` query parameter, else bodyRevision (the `revision` field
// of a JSON body, 0 when absent). A write without one is a 400, so a client can never overwrite
// blindly (PADR "Revisioned writes"). It writes the 400 and returns false when none is usable.
func revisionOf(c *gin.Context, bodyRevision int64) (int64, bool) {
	if h := strings.TrimSpace(c.GetHeader("If-Match")); h != "" {
		h = strings.TrimPrefix(strings.Trim(h, `"`), "r")
		if n, err := strconv.ParseInt(h, 10, 64); err == nil && n >= 1 {
			return n, true
		}
		badRequest(c, "If-Match must be the revision as returned in ETag")
		return 0, false
	}
	if v := c.Query("revision"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 1 {
			return n, true
		}
		badRequest(c, "revision must be a positive integer")
		return 0, false
	}
	if bodyRevision >= 1 {
		return bodyRevision, true
	}
	badRequest(c, "the current revision is required: send If-Match, a revision query parameter, or a revision field")
	return 0, false
}
