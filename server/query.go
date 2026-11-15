package server

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
)

// Field value types for filter coercion. A query string carries only text, so the resource says
// which of its filterable fields are not strings.
const (
	typeBool = "bool"
	typeInt  = "int"
)

// parseQuery reads the list conventions (PADR-0030) from the request: limit, cursor, sort
// (comma-separated field names, a leading "-" for descending), fields (projection), and
// filter[field]=value, filter[field][in]=a,b, filter[field][contains]=text. It checks shapes only;
// which fields may be filtered, sorted or projected is decided by the resource's data.Spec in the
// data layer, which rejects anything else. types names the non-string filterable fields. It writes
// the 400 and returns false on a malformed value.
func parseQuery(c *gin.Context, types map[string]string) (data.Query, bool) {
	q := data.Query{Cursor: c.Query("cursor")}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(c, "limit must be a positive integer")
			return q, false
		}
		q.Limit = n
	}
	for _, f := range splitList(c.Query("sort")) {
		q.Sort = append(q.Sort, data.SortKey{Field: strings.TrimPrefix(f, "-"), Desc: strings.HasPrefix(f, "-")})
	}
	q.Fields = splitList(c.Query("fields"))
	for key, values := range c.Request.URL.Query() {
		field, op, ok := filterKey(key)
		if !ok {
			continue
		}
		f := data.Filter{Field: field, Op: op}
		for _, raw := range values {
			parts := []string{raw}
			if op == data.OpIn {
				parts = splitList(raw)
			}
			for _, p := range parts {
				v, ok := coerce(p, types[field])
				if !ok {
					badRequest(c, "filter "+field+" has a value of the wrong type")
					return q, false
				}
				f.Values = append(f.Values, v)
			}
		}
		q.Filters = append(q.Filters, f)
	}
	return q, true
}

// filterKey parses filter[field] and filter[field][op].
func filterKey(key string) (field string, op data.Op, ok bool) {
	rest, found := strings.CutPrefix(key, "filter[")
	if !found {
		return "", "", false
	}
	field, rest, found = strings.Cut(rest, "]")
	if !found || field == "" {
		return "", "", false
	}
	switch rest {
	case "":
		return field, data.OpEq, true
	case "[in]":
		return field, data.OpIn, true
	case "[contains]":
		return field, data.OpContains, true
	}
	return "", "", false
}

func coerce(s, typ string) (any, bool) {
	switch typ {
	case typeBool:
		b, err := strconv.ParseBool(s)
		return b, err == nil
	case typeInt:
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	return s, true
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
