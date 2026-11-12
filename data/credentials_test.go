package data

import (
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func newCreds(t *testing.T) (*Credentials, *fake.DB) {
	t.Helper()
	db := fake.NewDB()
	r := NewCredentials(db)
	n := 0
	r.newID = func() string { n++; return testID(string(rune('a' + n - 1))) }
	r.now = func() time.Time { return clock }
	return r, db
}

func addCred(t *testing.T, r *Credentials, owner, hash string) *IntegrationCredential {
	t.Helper()
	c := &IntegrationCredential{Label: "foundry", Scopes: []string{"read"}, SecretHash: hash}
	if err := r.Create(ctx, owner, owner, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAuthenticateStopsAfterRevoke(t *testing.T) {
	r, _ := newCreds(t)
	c := addCred(t, r, "u1", "h1")
	if got, err := r.Authenticate(ctx, "h1"); err != nil || got.ID != c.ID || got.LastUsedAt == nil {
		t.Fatalf("before revoke: %+v, %v", got, err)
	}

	if _, err := r.Revoke(ctx, "u1", "u1", c.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Authenticate(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revoke = %v, want ErrNotFound", err)
	}
	if _, err := r.Authenticate(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty hash = %v, want ErrNotFound", err)
	}
}

func TestRevokeClassifiesOwnershipAndIsRepeatable(t *testing.T) {
	r, _ := newCreds(t)
	c := addCred(t, r, "u1", "h1")

	if _, err := r.Revoke(ctx, "u2", "u2", c.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("other owner = %v, want ErrForbidden", err)
	}
	if _, err := r.Revoke(ctx, "u1", "u1", testID("z")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown = %v, want ErrNotFound", err)
	}
	if _, err := r.Revoke(ctx, "u1", "u1", "junk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed = %v, want ErrNotFound", err)
	}
	if _, err := r.Revoke(ctx, "", "", c.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("no owner = %v, want ErrInvalid", err)
	}
	first, err1 := r.Revoke(ctx, "u1", "u1", c.ID)
	again, err2 := r.Revoke(ctx, "u1", "u1", c.ID)
	if err1 != nil || err2 != nil || first.RevokedAt == nil || again.Revision != first.Revision {
		t.Fatalf("repeat revoke: %+v %v / %+v %v", first, err1, again, err2)
	}
}

func TestCountActiveAndItemStatesIgnoreRevoked(t *testing.T) {
	r, _ := newCreds(t)
	a := addCred(t, r, "u1", "h1")
	addCred(t, r, "u1", "h2")
	addCred(t, r, "u2", "h3")
	if _, err := r.Revoke(ctx, "u1", "u1", a.ID); err != nil {
		t.Fatal(err)
	}

	n, err := r.CountActive(ctx, "u1")
	states, err2 := r.ItemStates(ctx, "u1")

	if err != nil || err2 != nil || n != 1 || len(states) != 1 {
		t.Fatalf("count = %d (%v), states = %d (%v); want 1 and 1", n, err, len(states), err2)
	}
	if _, err := r.CountActive(ctx, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank owner = %v, want ErrInvalid", err)
	}
}

func TestRevokeAllByOwnerEndsOnlyThatOwnersCredentials(t *testing.T) {
	r, _ := newCreds(t)
	addCred(t, r, "u1", "h1")
	addCred(t, r, "u1", "h2")
	addCred(t, r, "u2", "h3")

	n, err := r.RevokeAllByOwner(ctx, "u1", ReasonOwnerDenied)

	if err != nil || n != 2 {
		t.Fatalf("ended %d, %v; want 2", n, err)
	}
	if _, err := r.Authenticate(ctx, "h3"); err != nil {
		t.Fatalf("other owner's credential = %v, want still live", err)
	}
	if got, _ := r.RevokeAllByOwner(ctx, "u1", ReasonOwnerDenied); got != 0 {
		t.Fatalf("second pass ended %d, want 0", got)
	}
	for _, bad := range [][2]string{{"", "r"}, {"u1", " "}} {
		if _, err := r.RevokeAllByOwner(ctx, bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("RevokeAllByOwner(%q,%q) = %v, want ErrInvalid", bad[0], bad[1], err)
		}
	}
}

func TestGateDefaultsOpenAndPersists(t *testing.T) {
	r, _ := newCreds(t)
	if closed, err := r.GateClosed(ctx); err != nil || closed {
		t.Fatalf("fresh gate closed = %v, %v; want open", closed, err)
	}

	for _, want := range []bool{true, false, true} {
		if err := r.SetGate(ctx, want, "actor"); err != nil {
			t.Fatal(err)
		}
		if got, err := r.GateClosed(ctx); err != nil || got != want {
			t.Fatalf("closed = %v, %v; want %v", got, err, want)
		}
	}
}

func TestListAllSpansOwnersAndListOwnedValidatesTheOwner(t *testing.T) {
	r, _ := newCreds(t)
	addCred(t, r, "u1", "h1")
	addCred(t, r, "u2", "h2")

	all, err := r.ListAll(ctx, Query{})
	one, err2 := r.ListOwned(ctx, "u2", Query{})

	if err != nil || err2 != nil || len(all.Items) != 2 || len(one.Items) != 1 || one.Items[0].OwnerUserID != "u2" {
		t.Fatalf("all=%d one=%d errs=%v %v", len(all.Items), len(one.Items), err, err2)
	}
	if _, err := r.ListOwned(ctx, `{"$ne":1}`, Query{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operator-shaped owner = %v, want ErrInvalid", err)
	}
	if _, err := r.Deletions(ctx, "a b", Query{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad owner on deletions = %v, want ErrInvalid", err)
	}
}

func TestDeletePermanentlyRemovesAndRecordsOnce(t *testing.T) {
	r, db := newCreds(t)
	c := addCred(t, r, "u1", "h1")

	entry, err := r.DeletePermanently(ctx, "admin", c.ID, "  leaked ")

	if err != nil || entry.Reason != "leaked" || entry.OwnerUserID != "u1" || entry.CreatedBy != "admin" || entry.Label != "foundry" {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	if n := len(db.Fake(CollectionCredentials).Docs()); n != 0 {
		t.Fatalf("credentials left = %d", n)
	}
	page, err := r.Deletions(ctx, "", Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("deletions = %v, %v", page.Items, err)
	}
	if _, err := r.DeletePermanently(ctx, "admin", "junk", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v, want ErrNotFound", err)
	}
}

func TestLiveRejectsRevokedDeletedAndMalformed(t *testing.T) {
	r, _ := newCreds(t)
	live := addCred(t, r, "u1", "h1")
	gone := addCred(t, r, "u1", "h2")
	if _, err := r.Revoke(ctx, "u1", "u1", gone.ID); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]bool{live.ID: true, gone.ID: false, testID("z"): false, "junk": false} {
		if got, err := r.Live(ctx, id); err != nil || got != want {
			t.Errorf("Live(%q) = %v, %v; want %v", id, got, err, want)
		}
	}
}
