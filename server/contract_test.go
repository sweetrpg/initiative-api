package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sweetrpg/initiative-api/store/fake"
)

// The HTTP error contract: every typed error a client can see, with its status and body, pinned
// in testdata/contract/http_errors.json. Clients load that file as a fixture. Regenerate after an
// intentional change with UPDATE_FIXTURES=1 go test ./server -run Contract and review the diff.

const httpFixture = "../testdata/contract/http_errors"

type contractCase struct {
	Scenario string         `json:"scenario"`
	Status   int            `json:"status"`
	Body     map[string]any `json:"body"`
}

// normalize replaces values that differ between runs, at any depth.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch k {
			case "created_at", "updated_at", "last_used_at", "revoked_at", "deleted_at":
				if val != nil { // null is part of the contract and stays null
					x[k] = "TIMESTAMP"
				}
			case "id", "credential_id":
				x[k] = "GENERATED_ID"
			case "correlation_id":
				x[k] = "CORRELATION_ID"
			default:
				x[k] = normalize(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
	}
	return v
}

func record(t *testing.T, scenario string, w *httptest.ResponseRecorder) contractCase {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q: %v", scenario, w.Body.String(), err)
	}
	return contractCase{Scenario: scenario, Status: w.Code, Body: normalize(body).(map[string]any)}
}

func contractCases(t *testing.T) []contractCase {
	t.Helper()
	var out []contractCase
	add := func(s string, w *httptest.ResponseRecorder) { out = append(out, record(t, s, w)) }

	themes := themeRouter(t, 1, 200)
	add("missing token", func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		themes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/themes", nil))
		return w
	}())
	add("token without a role", callAs(themes, "no-role", "GET", "/themes", ""))
	add("denied account", callAs(themes, "denied", "GET", "/themes", ""))
	add("bad limit parameter", callAs(themes, "user-ok", "GET", "/themes?limit=abc", ""))
	add("invalid theme", callAs(themes, "user-ok", "POST", "/themes", `{"name":"A","main_color":"blue"}`))
	add("theme not found", callAs(themes, "user-ok", "GET", "/themes/64b7f0f0f0f0f0f0f0f0f0f0", ""))
	add("missing revision", callAs(themes, "user-ok", "PATCH", "/themes/x", `{"name":"n"}`))
	first := callAs(themes, "user-ok", "POST", "/themes", themeJSON("A"))
	add("theme created", first)
	add("limit reached", callAs(themes, "user-ok", "POST", "/themes", themeJSON("B")))
	id := decodeBody(t, first)["id"].(string)
	add("stale revision", callAs(themes, "user-ok", "PATCH", "/themes/"+id, `{"revision":9,"name":"x"}`))
	add("another user's theme", callAs(themes, "user-two", "PATCH", "/themes/"+id, `{"revision":1,"name":"x"}`))
	packaged := decodeBody(t, callAs(themes, "user-ok", "GET", "/themes/packaged", ""))["items"].([]any)[0].(map[string]any)["id"].(string)
	add("packaged theme edit", callAs(themes, "user-ok", "PATCH", "/themes/"+packaged, `{"revision":1,"name":"x"}`))
	down := themeRouter(t, 1, http.StatusBadGateway)
	add("entitlements down on create", callAs(down, "user-ok", "POST", "/themes", themeJSON("A")))

	creds := credRouter(t, "3")
	credID, secret := issue(t, creds, "user-ok", `["read"]`)
	add("integration credential on an account route", callAs(creds, secret, "POST", "/credentials", `{"label":"x","scopes":["read"]}`))
	add("credential lacks write scope", callAs(creds, secret, "POST", "/themes", themeJSON("A")))
	add("unknown credential secret", callAs(creds, "ic_unknown", "GET", "/probe", ""))
	add("revoke another user's credential", callAs(creds, "user-two", "POST", "/credentials/"+credID+"/revoke", ""))
	add("invalid credential", callAs(creds, "user-ok", "POST", "/credentials", `{"label":"","scopes":["read"]}`))
	add("admin route as a user", callAs(creds, "user-two", "GET", "/admin/credentials", ""))
	add("admin delete without a reason", callAs(creds, "admin-ok", "DELETE", "/admin/credentials/"+credID, `{}`))
	add("admin delete unknown credential", callAs(creds, "admin-ok", "DELETE", "/admin/credentials/64b7f0f0f0f0f0f0f0f0f0f0", `{"reason":"x"}`))
	limit1 := credRouter(t, "1")
	issue(t, limit1, "user-ok", `["read"]`)
	add("credential limit reached", callAs(limit1, "user-ok", "POST", "/credentials", `{"label":"two","scopes":["read"]}`))

	closed, svc := credRouterWith(t, "3")
	_, closedSecret := issue(t, closed, "user-ok", `["read"]`)
	if err := svc.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	add("credentials closed", callAs(closed, closedSecret, "GET", "/probe", ""))

	broken, _ := credRouterDB(t, "3", failingDelete{fake.NewDB()})
	brokenID, _ := issue(t, broken, "user-two", `["read"]`)
	add("admin delete did not complete", callAs(broken, "admin-ok", "DELETE", "/admin/credentials/"+brokenID, `{"reason":"x"}`))
	return out
}

func themeJSON(name string) string {
	b, _ := json.Marshal(theme(name))
	return string(b)
}

func TestContractHTTPErrors(t *testing.T) {
	cases := contractCases(t)
	got, err := json.MarshalIndent(map[string]any{"cases": cases}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := httpFixture + ".json"
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		if os.Getenv("CI") != "" {
			t.Fatal("UPDATE_FIXTURES is set in CI; fixtures are never rewritten there")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed fixture path
	if err != nil {
		t.Fatalf("missing fixture (run with UPDATE_FIXTURES=1 locally): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("HTTP error contract drifted from %s:\n got: %s\nwant: %s", path, got, want)
	}
}

func TestContractCasesAreUniqueAndTyped(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range contractCases(t) {
		if seen[c.Scenario] {
			t.Errorf("duplicate scenario %q", c.Scenario)
		}
		seen[c.Scenario] = true
		if c.Status >= 400 {
			// authz-client.go's middleware answers 401/403 as {"error", "message"}; every other
			// error is {"code", "message", "correlation_id"}. Both are pinned in the fixture.
			code, _ := c.Body["code"].(string)
			if code == "" {
				code, _ = c.Body["error"].(string)
			}
			if code == "" || strings.TrimSpace(fmt.Sprint(c.Body["message"])) == "" {
				t.Errorf("%s: error body has no code and message: %v", c.Scenario, c.Body)
			}
		}
	}
}
