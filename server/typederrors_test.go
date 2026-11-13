package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func typedRouter(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Correlation(), typedErrors())
	r.GET("/x", handler)
	return r
}

func serve(r http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	return w
}

func TestTypedErrorsRewriteTheLibraryShape(t *testing.T) {
	r := typedRouter(func(c *gin.Context) {
		c.Header("Content-Length", "999")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden", "message": "no role"})
	})

	w := serve(r)

	b := decodeBody(t, w)
	if b["code"] != "forbidden" || b["message"] != "no role" || b["error"] != nil || b["correlation_id"] == "" {
		t.Fatalf("body = %v, want the typed shape with a correlation id", b)
	}
	if got := w.Header().Get("Content-Length"); got == "999" {
		t.Fatalf("stale Content-Length %q survived the rewrite", got)
	}
}

func TestTypedErrorsLeaveTypedBodiesAndSuccessAlone(t *testing.T) {
	typed := `{"code":"not_found","message":"Not found.","correlation_id":"c1"}`
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"typed 403":     {http.StatusForbidden, typed},
		"typed 503":     {http.StatusServiceUnavailable, typed},
		"success body":  {http.StatusOK, `{"error":"x","message":"not an error"}`},
		"404 old shape": {http.StatusNotFound, `{"error":"x","message":"outside the rewritten statuses"}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := typedRouter(func(c *gin.Context) {
				c.Data(tc.status, "application/json", []byte(tc.body))
			})

			w := serve(r)

			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("got %d %s, want %d %s unchanged", w.Code, w.Body, tc.status, tc.body)
			}
		})
	}
}
