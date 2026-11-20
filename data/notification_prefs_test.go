package data

import (
	"context"
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func TestPreferencesAreKeyedByUserAndSessionAndRejectUnknownTypes(t *testing.T) {
	r := NewNotificationPrefs(fake.NewDB())
	ctx := context.Background()
	if _, err := r.SetMuted(ctx, "u-1", "s-1", []string{NoticeTurnNext, NoticeTurnNext, NoticeTurnYours}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Muted(ctx, "u-1", "s-1"); !slices.Equal(got, []string{NoticeTurnNext, NoticeTurnYours}) {
		t.Errorf("u-1 in s-1 = %v, want both types once", got)
	}
	if got, _ := r.Muted(ctx, "u-2", "s-1"); len(got) != 0 {
		t.Errorf("another user sees %v", got)
	}
	if got, _ := r.Muted(ctx, "u-1", "s-2"); len(got) != 0 {
		t.Errorf("another session sees %v", got)
	}
	if _, err := r.SetMuted(ctx, "u-1", "s-1", []string{"nope"}); err == nil {
		t.Error("an unknown type was accepted")
	}
	if got, _ := r.Muted(ctx, "u-1", "s-1"); len(got) != 2 {
		t.Errorf("a rejected write changed the stored list: %v", got)
	}
}
