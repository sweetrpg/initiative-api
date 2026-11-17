package domain

import (
	"testing"
	"time"
)

func TestTheOwnerRecordMaxAgeFollowsTheConfiguredValueAndDefaultsToSevenDays(t *testing.T) {
	s := &Sessions{}
	if got := s.ownerRecordMaxAge(); got != 7*24*time.Hour {
		t.Fatalf("unset = %v, want 168h", got)
	}

	s.OwnerRecordMaxAge = 36 * time.Hour

	if got := s.ownerRecordMaxAge(); got != 36*time.Hour {
		t.Fatalf("configured = %v, want 36h", got)
	}
}
