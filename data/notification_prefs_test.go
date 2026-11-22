package data

import (
	"context"
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/store/fake"
)

const (
	sess1 = "5f00000000000000000000a1"
	sess2 = "5f00000000000000000000a2"
)

func TestPreferencesAreKeyedByUserAndSessionAndRejectUnknownTypes(t *testing.T) {
	r := NewNotificationPrefs(fake.NewDB())
	ctx := context.Background()
	if _, err := r.SetMuted(ctx, "u-1", sess1, []string{NoticeTurnNext, NoticeTurnNext, NoticeTurnYours}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Muted(ctx, "u-1", sess1); !slices.Equal(got, []string{NoticeTurnNext, NoticeTurnYours}) {
		t.Errorf("u-1 in s-1 = %v, want both types once", got)
	}
	if got, _ := r.Muted(ctx, "u-2", sess1); len(got) != 0 {
		t.Errorf("another user sees %v", got)
	}
	if got, _ := r.Muted(ctx, "u-1", sess2); len(got) != 0 {
		t.Errorf("another session sees %v", got)
	}
	if _, err := r.SetMuted(ctx, "u-1", "not-an-id", nil); err == nil {
		t.Error("a malformed session id was accepted")
	}
	if _, err := r.SetMuted(ctx, "u-1", sess1, []string{"nope"}); err == nil {
		t.Error("an unknown type was accepted")
	}
	if got, _ := r.Muted(ctx, "u-1", sess1); len(got) != 2 {
		t.Errorf("a rejected write changed the stored list: %v", got)
	}
}

func TestAMuteListLongerThanTheKnownTypesIsRejectedBeforeAnyWork(t *testing.T) {
	r := NewNotificationPrefs(fake.NewDB())
	long := make([]string, len(NoticeTypes)+1)
	for i := range long {
		long[i] = NoticeTurnNext
	}
	if _, err := r.SetMuted(context.Background(), "u-1", sess1, long); err == nil {
		t.Error("an over-long list was accepted")
	}
}
