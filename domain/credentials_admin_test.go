package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func newCreds(t *testing.T) (*Credentials, *fake.DB) {
	t.Helper()
	db := fake.NewDB()
	s := NewCredentials(db)
	if _, err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestLiveFollowsRevokeAndPermanentDelete(t *testing.T) {
	s, _ := newCreds(t)
	ok := stubChecker{limit: 5}
	revoked, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, ok)
	deleted, _, _ := s.Create(ctx, "u1", "b", []string{"read"}, ok)
	kept, _, _ := s.Create(ctx, "u1", "c", []string{"read"}, ok)
	if _, err := s.Revoke(ctx, "u1", revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletePermanently(ctx, "admin", deleted.ID, "leaked"); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]bool{revoked.ID: false, deleted.ID: false, kept.ID: true, "junk": false} {
		got, err := s.Live(ctx, id)
		if err != nil || got != want {
			t.Errorf("Live(%q) = %v, %v; want %v", id, got, err, want)
		}
	}
}

func TestLiveIsFalseWhileGateClosed(t *testing.T) {
	s, _ := newCreds(t)
	c, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Live(ctx, c.ID); got {
		t.Fatal("a closed gate must end open streams")
	}
}

func TestPermanentDeleteRetryWritesOneAuditEntry(t *testing.T) {
	s, db := newCreds(t)
	c, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	if _, err := s.DeletePermanently(ctx, "admin", c.ID, "first"); err != nil {
		t.Fatal(err)
	}

	_, err := s.DeletePermanently(ctx, "admin", c.ID, "again")

	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if n := len(db.Fake(data.CollectionCredentialDeletions).Docs()); n != 1 {
		t.Fatalf("audit entries = %d, want 1", n)
	}
}

func TestPermanentDeleteResumesAfterAuditWritten(t *testing.T) {
	s, db := newCreds(t)
	c, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	pre := data.CredentialDeletion{CredentialID: c.ID, OwnerUserID: "u1", Reason: "first try", CreatedBy: "admin"}
	if err := db.Collection(data.CollectionCredentialDeletions).InsertOne(ctx, pre); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeletePermanently(ctx, "admin", c.ID, "retry"); err != nil {
		t.Fatalf("retry = %v, want the deletion to finish", err)
	}

	if n := len(db.Fake(data.CollectionCredentials).Docs()); n != 0 {
		t.Fatalf("credentials left = %d, want 0", n)
	}
	if n := len(db.Fake(data.CollectionCredentialDeletions).Docs()); n != 1 {
		t.Fatalf("audit entries = %d, want 1", n)
	}
}

func TestPermanentDeleteRejectsBlankActorOrReason(t *testing.T) {
	s, _ := newCreds(t)
	c, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	for _, tc := range [][2]string{{"", "why"}, {"admin", " "}} {
		if _, err := s.DeletePermanently(ctx, tc[0], c.ID, tc[1]); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("DeletePermanently(%q, %q) = %v, want ErrInvalid", tc[0], tc[1], err)
		}
	}
	if got, _ := s.Live(ctx, c.ID); !got {
		t.Fatal("a rejected deletion must leave the credential alone")
	}
}

func TestListAllSpansOwnersAndHidesNothingRevoked(t *testing.T) {
	s, _ := newCreds(t)
	a, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	if _, _, err := s.Create(ctx, "u2", "b", []string{"read"}, stubChecker{limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke(ctx, "u1", a.ID); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAll(ctx, "", data.Query{})
	one, _ := s.ListAll(ctx, "u1", data.Query{})

	if err != nil || len(all.Items) != 2 || len(one.Items) != 1 {
		t.Fatalf("all = %d, one = %d, err = %v; want 2, 1", len(all.Items), len(one.Items), err)
	}
}

func TestDeletionsFiltersByOwner(t *testing.T) {
	s, _ := newCreds(t)
	a, _, _ := s.Create(ctx, "u1", "a", []string{"read"}, stubChecker{limit: 5})
	b, _, _ := s.Create(ctx, "u2", "b", []string{"read"}, stubChecker{limit: 5})
	for id, why := range map[string]string{a.ID: "x", b.ID: "y"} {
		if _, err := s.DeletePermanently(ctx, "admin", id, why); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Deletions(ctx, "u2")

	if err != nil || len(got) != 1 || got[0].CredentialID != b.ID {
		t.Fatalf("got %v, %v", got, err)
	}
}
