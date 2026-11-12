package domain

import (
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
)

func TestPlan(t *testing.T) {
	// a, b, c, d are oldest to newest.
	st := func(id string, rank int, inactive bool) data.ItemState {
		return data.ItemState{ID: id, KeepRank: rank, Inactive: inactive}
	}
	tests := []struct {
		name       string
		states     []data.ItemState
		limit      int
		act, deact []string
	}{
		{"within the limit changes nothing", []data.ItemState{st("a", 0, false), st("b", 0, false)}, 2, nil, nil},
		{"downgrade keeps the oldest", []data.ItemState{st("a", 0, false), st("b", 0, false), st("c", 0, false)}, 1, nil, []string{"b", "c"}},
		{"choice beats age", []data.ItemState{st("a", 0, false), st("b", 0, false), st("c", 1, false)}, 1, nil, []string{"a", "b"}},
		{"choice order then oldest on upgrade", []data.ItemState{st("a", 0, true), st("b", 0, true), st("c", 2, true), st("d", 1, false)}, 3, []string{"c", "a"}, nil},
		{"limit zero deactivates everything", []data.ItemState{st("a", 0, false)}, 0, nil, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act, deact := plan(tt.states, tt.limit)
			if !slices.Equal(act, tt.act) || !slices.Equal(deact, tt.deact) {
				t.Fatalf("activate %v deactivate %v, want %v and %v", act, deact, tt.act, tt.deact)
			}
		})
	}
}
