package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/common.go/logging"
)

func TestMain(m *testing.M) {
	logging.Init()
	os.Exit(m.Run())
}

// fakeAuthAPI stands in for auth-api's /authz/check: it rejects tokens that fail verification
// (expired, bad signature, wrong aud/iss, or a client-credentials token) with 401 and answers
// verified user tokens with their roles.
func fakeAuthAPI(t *testing.T) *httptest.Server {
	t.Helper()
	verdicts := map[string]authz.CheckResponse{
		"user-ok":  {Allowed: true, Roles: []string{authz.RoleUser}, Sub: "auth0|1"},
		"user-two": {Allowed: true, Roles: []string{authz.RoleUser}, Sub: "auth0|5"},
		"no-role":  {Allowed: true, Roles: []string{}, Sub: "auth0|2"},
		"admin-ok": {Allowed: true, Roles: []string{authz.RoleAdmin}, Sub: "auth0|3"},
		"denied":   {Allowed: false, Reason: "denied", Sub: "auth0|4"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")[len("Bearer "):]
		v, ok := verdicts[token]
		if !ok { // expired, bad-signature, wrong-aud, wrong-iss, client-only
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeUsersAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "u-1"
		if r.Header.Get("Authorization") == "Bearer user-two" {
			id = "u-2"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"user_id": id})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func protectedRouter(t *testing.T, roles ...string) (*gin.Engine, *int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	client := authz.NewClient(fakeAuthAPI(t).URL, fakeUsersAPI(t).URL)
	r := gin.New()
	g := ProtectedGroup(r, client, nil, roles...)
	reached := 0
	// A route added after the group is created, with no auth code of its own.
	g.GET("/probe", func(c *gin.Context) {
		reached++
		c.JSON(http.StatusOK, gin.H{"viewer": authz.Viewer(c)})
	})
	return r, &reached
}

func get(r http.Handler, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestProtectedGroupStatus(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		auth  string
		want  int
	}{
		{"valid user passes", nil, "Bearer user-ok", http.StatusOK},
		{"missing header", nil, "", http.StatusUnauthorized},
		{"non-bearer scheme", nil, "Basic user-ok", http.StatusUnauthorized},
		{"expired token", nil, "Bearer expired", http.StatusUnauthorized},
		{"bad signature", nil, "Bearer bad-signature", http.StatusUnauthorized},
		{"wrong audience", nil, "Bearer wrong-aud", http.StatusUnauthorized},
		{"wrong issuer", nil, "Bearer wrong-iss", http.StatusUnauthorized},
		{"client-only identity", nil, "Bearer client-credentials", http.StatusUnauthorized},
		{"verified user without required role", nil, "Bearer no-role", http.StatusForbidden},
		{"denied by auth-api", nil, "Bearer denied", http.StatusForbidden},
		{"user lacks admin role", []string{authz.RoleAdmin}, "Bearer user-ok", http.StatusForbidden},
		{"admin holds admin role", []string{authz.RoleAdmin}, "Bearer admin-ok", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := protectedRouter(t, tt.roles...)
			if got := get(r, tt.auth).Code; got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRejectedRequestsNeverReachHandler(t *testing.T) {
	for _, auth := range []string{"", "Bearer expired", "Bearer no-role"} {
		t.Run(auth, func(t *testing.T) {
			r, reached := protectedRouter(t)
			get(r, auth)
			if *reached != 0 {
				t.Fatalf("handler ran %d times for %q", *reached, auth)
			}
		})
	}
}

func TestValidUserIsAttributedToHandler(t *testing.T) {
	r, _ := protectedRouter(t)
	w := get(r, "Bearer user-ok")
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["viewer"] != "u-1" {
		t.Fatalf("viewer = %q, want u-1", body["viewer"])
	}
}

func TestAuthAPIOutageFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ProtectedGroup(r, authz.NewClient("http://127.0.0.1:1", ""), nil).GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	if got := get(r, "Bearer user-ok").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
}
