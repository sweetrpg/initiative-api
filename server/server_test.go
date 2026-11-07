package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/constants"
)

func newRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Correlation())
	SetupHandlers(r, authz.NewClient("http://127.0.0.1:1", "http://127.0.0.1:1"))
	return r
}

func TestStatusEndpoints(t *testing.T) {
	tests := []struct {
		path     string
		wantBody string
	}{
		{"/status/ping", "hostname"},
		{"/status/health", "errors"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			newRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !strings.Contains(strings.ToLower(w.Body.String()), tt.wantBody) {
				t.Fatalf("body %q missing %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestCorrelationHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		same bool
	}{
		{"generated when absent", "", false},
		{"propagated when present", "abc-123", true},
		{"replaced when oversized", strings.Repeat("x", 129), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status/ping", nil)
			if tt.in != "" {
				req.Header.Set(constants.CorrelationHeader, tt.in)
			}
			w := httptest.NewRecorder()
			newRouter().ServeHTTP(w, req)
			got := w.Header().Get(constants.CorrelationHeader)
			if got == "" || (got == tt.in) != tt.same {
				t.Fatalf("correlation id = %q (in %q, same=%v)", got, tt.in, tt.same)
			}
		})
	}
}
