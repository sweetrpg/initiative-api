package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminListsAnyOwnersCredentialsWithoutSecrets(t *testing.T) {
	r := credRouter(t, "3")
	_, secret := issue(t, r, "user-ok", `["read"]`)
	issue(t, r, "user-two", `["write"]`)

	all := callAs(r, "admin-ok", "GET", "/admin/credentials", "")
	one := callAs(r, "admin-ok", "GET", "/admin/credentials?owner=u-2", "")

	if n := len(decodeBody(t, all)["items"].([]any)); n != 2 {
		t.Fatalf("all = %d items, want 2", n)
	}
	if n := len(decodeBody(t, one)["items"].([]any)); n != 1 {
		t.Fatalf("one owner = %d items, want 1", n)
	}
	if strings.Contains(all.Body.String(), secret) || strings.Contains(all.Body.String(), "secret_hash") {
		t.Fatalf("list leaks a secret: %s", all.Body)
	}
}

func TestAdminDeleteEndsCredentialAtOnceAndFreesSlot(t *testing.T) {
	r := credRouter(t, "1")
	id, secret := issue(t, r, "user-ok", `["read"]`)

	w := callAs(r, "admin-ok", "DELETE", "/admin/credentials/"+id, `{"reason":"leaked in a public repo"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if w := callAs(r, secret, "GET", "/probe", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("deleted credential = %d, want 401 on the next request", w.Code)
	}
	if w := callAs(r, "user-ok", "POST", "/credentials", `{"label":"new","scopes":["read"]}`); w.Code != http.StatusCreated {
		t.Fatalf("create after delete = %d, want the slot free", w.Code)
	}
}

func TestAdminDeleteAuditEntryOutlivesCredential(t *testing.T) {
	r := credRouter(t, "3")
	id, _ := issue(t, r, "user-two", `["read","write"]`)
	callAs(r, "admin-ok", "DELETE", "/admin/credentials/"+id, `{"reason":"compromised"}`)

	w := callAs(r, "admin-ok", "GET", "/admin/credentials/deletions?owner=u-2", "")

	items := decodeBody(t, w)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("deletions = %d, want 1: %s", len(items), w.Body)
	}
	e := items[0].(map[string]any)
	if e["credential_id"] != id || e["owner_user_id"] != "u-2" || e["deleted_by"] != "u-1" ||
		e["label"] != "foundry" || e["reason"] != "compromised" || len(e["scopes"].([]any)) != 2 || e["deleted_at"] == nil {
		t.Fatalf("entry = %v", e)
	}
	if w := callAs(r, "user-two", "GET", "/credentials", ""); len(decodeBody(t, w)["items"].([]any)) != 0 {
		t.Fatalf("credential still listed after deletion")
	}
}

func TestAdminDeleteNeedsReasonAndDeletesNothingWithout(t *testing.T) {
	r := credRouter(t, "3")
	id, secret := issue(t, r, "user-ok", `["read"]`)

	for _, body := range []string{``, `{}`, `{"reason":"   "}`} {
		if w := callAs(r, "admin-ok", "DELETE", "/admin/credentials/"+id, body); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q = %d, want 422", body, w.Code)
		}
	}

	if w := callAs(r, secret, "GET", "/probe", ""); w.Code != http.StatusOK {
		t.Fatalf("credential after rejected deletes = %d, want still live", w.Code)
	}
}

func TestAdminCredentialRoutesRejectNonAdmins(t *testing.T) {
	r := credRouter(t, "3")
	id, secret := issue(t, r, "user-ok", `["read"]`)
	routes := [][3]string{
		{"GET", "/admin/credentials", ""},
		{"GET", "/admin/credentials/deletions", ""},
		{"DELETE", "/admin/credentials/" + id, `{"reason":"x"}`},
	}
	for _, rt := range routes {
		if w := callAs(r, "user-two", rt[0], rt[1], rt[2]); w.Code != http.StatusForbidden {
			t.Fatalf("%s %s as user = %d, want 403", rt[0], rt[1], w.Code)
		}
		if w := callAs(r, secret, rt[0], rt[1], rt[2]); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a credential = %d, want 401", rt[0], rt[1], w.Code)
		}
	}
	if w := callAs(r, "user-ok", "GET", "/credentials", ""); len(decodeBody(t, w)["items"].([]any)) != 1 {
		t.Fatal("a rejected DELETE removed the credential")
	}
	if w := callAs(r, "admin-ok", "GET", "/admin/credentials/deletions", ""); len(decodeBody(t, w)["items"].([]any)) != 0 {
		t.Fatal("a rejected DELETE wrote an audit entry")
	}
}

func TestAdminDeleteUnknownCredentialIs404(t *testing.T) {
	r := credRouter(t, "3")
	if w := callAs(r, "admin-ok", "DELETE", "/admin/credentials/64b7f0f0f0f0f0f0f0f0f0f0", `{"reason":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", w.Code)
	}
	if w := callAs(r, "admin-ok", "DELETE", "/admin/credentials/not-an-id", `{"reason":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", w.Code)
	}
}

func TestAdminListRejectsBadLimit(t *testing.T) {
	r := credRouter(t, "3")
	if w := callAs(r, "admin-ok", "GET", "/admin/credentials?limit=abc", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", w.Code)
	}
}

func TestAdminListPages(t *testing.T) {
	r := credRouter(t, "3")
	issue(t, r, "user-ok", `["read"]`)
	issue(t, r, "user-two", `["read"]`)

	first := decodeBody(t, callAs(r, "admin-ok", "GET", "/admin/credentials?limit=1", ""))
	next, _ := first["next_cursor"].(string)
	second := decodeBody(t, callAs(r, "admin-ok", "GET", "/admin/credentials?limit=1&cursor="+next, ""))

	if len(first["items"].([]any)) != 1 || next == "" || len(second["items"].([]any)) != 1 {
		t.Fatalf("first = %v, second = %v", first, second)
	}
}

func TestAdminOwnerFilterMustBeAPlainUserID(t *testing.T) {
	r := credRouter(t, "3")
	for _, path := range []string{"/admin/credentials?owner=%7B%22%24ne%22%3A1%7D", "/admin/credentials/deletions?owner=a%20b"} {
		if w := callAs(r, "admin-ok", "GET", path, ""); w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want a client error", path, w.Code)
		}
	}
}

func TestAdminDeletionsPage(t *testing.T) {
	r := credRouter(t, "5")
	for range 3 {
		id, _ := issue(t, r, "user-two", `["read"]`)
		callAs(r, "admin-ok", "DELETE", "/admin/credentials/"+id, `{"reason":"x"}`)
	}

	first := decodeBody(t, callAs(r, "admin-ok", "GET", "/admin/credentials/deletions?limit=2", ""))
	next, _ := first["next_cursor"].(string)
	second := decodeBody(t, callAs(r, "admin-ok", "GET", "/admin/credentials/deletions?limit=2&cursor="+next, ""))

	if len(first["items"].([]any)) != 2 || next == "" || len(second["items"].([]any)) != 1 || second["next_cursor"] != "" {
		t.Fatalf("first = %v, second = %v", first, second)
	}
}
