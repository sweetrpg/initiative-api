package domain

import (
	"slices"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
)

func f(v float64) *float64 { return &v }

func cbt(id, name, kind string, init *float64) data.Combatant {
	return data.Combatant{ID: id, Name: name, Kind: kind, Initiative: init}
}

func identity(n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return p
}

func reverse(n int) []int {
	p := identity(n)
	slices.Reverse(p)
	return p
}

func TestBuildQueue(t *testing.T) {
	removed := cbt("r", "Gone", data.KindPC, f(99))
	removed.Removed = true
	tests := []struct {
		name     string
		ordering string
		tie      string
		cs       []data.Combatant
		prev     []string
		perm     func(int) []int
		want     []string
	}{
		{"high to low", data.OrderHighToLow, data.TieQuery,
			[]data.Combatant{cbt("a", "A", data.KindPC, f(5)), cbt("b", "B", data.KindPC, f(18)), cbt("c", "C", data.KindPC, f(11))},
			nil, identity, []string{"b", "c", "a"}},
		{"low to high", data.OrderLowToHigh, data.TieQuery,
			[]data.Combatant{cbt("a", "A", data.KindPC, f(5)), cbt("b", "B", data.KindPC, f(18)), cbt("c", "C", data.KindPC, f(11))},
			nil, identity, []string{"a", "c", "b"}},
		{"unset initiative sorts last when low to high", data.OrderLowToHigh, data.TieQuery,
			[]data.Combatant{cbt("a", "A", data.KindPC, nil), cbt("b", "B", data.KindPC, f(3))},
			nil, identity, []string{"b", "a"}},
		{"unset initiative sorts last when high to low", data.OrderHighToLow, data.TieQuery,
			[]data.Combatant{cbt("a", "A", data.KindPC, nil), cbt("b", "B", data.KindPC, f(3)), cbt("c", "C", data.KindPC, f(9))},
			nil, identity, []string{"c", "b", "a"}},
		{"removed combatants are skipped", data.OrderHighToLow, data.TieQuery,
			[]data.Combatant{removed, cbt("a", "A", data.KindPC, f(1))},
			nil, identity, []string{"a"}},
		{"tie alphabetical ignores case", data.OrderHighToLow, data.TieAlphabetical,
			[]data.Combatant{cbt("1", "zed", data.KindPC, f(10)), cbt("2", "Amy", data.KindPC, f(10)), cbt("3", "bob", data.KindPC, f(10))},
			nil, identity, []string{"2", "3", "1"}},
		{"tie alphabetical falls back to id", data.OrderHighToLow, data.TieAlphabetical,
			[]data.Combatant{cbt("2", "Same", data.KindPC, f(10)), cbt("1", "Same", data.KindPC, f(10))},
			nil, identity, []string{"1", "2"}},
		{"tie query keeps list order", data.OrderHighToLow, data.TieQuery,
			[]data.Combatant{cbt("x", "X", data.KindPC, f(10)), cbt("y", "Y", data.KindPC, f(10))},
			nil, identity, []string{"x", "y"}},
		{"tie random uses the permutation", data.OrderHighToLow, data.TieRandom,
			[]data.Combatant{cbt("x", "X", data.KindPC, f(10)), cbt("y", "Y", data.KindPC, f(10)), cbt("z", "Z", data.KindPC, f(10))},
			nil, reverse, []string{"z", "y", "x"}},
		{"tie random keeps queued combatants in place and appends new ones", data.OrderHighToLow, data.TieRandom,
			[]data.Combatant{cbt("x", "X", data.KindPC, f(10)), cbt("y", "Y", data.KindPC, f(10)), cbt("n", "N", data.KindPC, f(10))},
			[]string{"y", "x"}, reverse, []string{"y", "x", "n"}},
		{"tie does not outrank initiative", data.OrderHighToLow, data.TieAlphabetical,
			[]data.Combatant{cbt("a", "A", data.KindPC, f(1)), cbt("z", "Z", data.KindPC, f(2))},
			nil, identity, []string{"z", "a"}},
		{"pc vs adversary groups then sorts", data.OrderPCVsAdversary, data.TieQuery,
			[]data.Combatant{
				cbt("g1", "Goblin", data.KindAdversary, f(20)), cbt("p1", "Pia", data.KindPC, f(3)),
				cbt("o1", "Door", data.KindObject, f(30)), cbt("p2", "Pat", data.KindPC, f(9))},
			nil, identity, []string{"p2", "p1", "g1", "o1"}},
		{"player managed keeps manual order and appends new", data.OrderPlayerManaged, data.TieRandom,
			[]data.Combatant{cbt("a", "A", data.KindPC, f(1)), cbt("b", "B", data.KindPC, f(99)), cbt("c", "C", data.KindPC, f(50))},
			[]string{"c", "gone", "a"}, identity, []string{"c", "a", "b"}},
		{"empty", data.OrderHighToLow, data.TieQuery, nil, nil, identity, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildQueue(tt.ordering, tt.tie, tt.cs, tt.prev, tt.perm)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("queue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExpireConditions(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   []data.Condition
		want []data.Condition
	}{
		{"one round left is removed", []data.Condition{{Name: "Stunned", RoundsRemaining: n(1)}}, []data.Condition{}},
		{"two rounds left ticks to one", []data.Condition{{Name: "Slowed", RoundsRemaining: n(2)}}, []data.Condition{{Name: "Slowed", RoundsRemaining: n(1)}}},
		{"open ended is untouched", []data.Condition{{Name: "Cursed"}}, []data.Condition{{Name: "Cursed"}}},
		{"mixed", []data.Condition{{Name: "A", RoundsRemaining: n(1)}, {Name: "B"}, {Name: "C", RoundsRemaining: n(3)}},
			[]data.Condition{{Name: "B"}, {Name: "C", RoundsRemaining: n(2)}}},
		{"none", nil, []data.Condition{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := data.Combatant{Conditions: tt.in}
			expireConditions(&c)
			if len(c.Conditions) != len(tt.want) {
				t.Fatalf("conditions = %+v, want %+v", c.Conditions, tt.want)
			}
			for i, w := range tt.want {
				g := c.Conditions[i]
				if g.Name != w.Name || (g.RoundsRemaining == nil) != (w.RoundsRemaining == nil) ||
					(w.RoundsRemaining != nil && *g.RoundsRemaining != *w.RoundsRemaining) {
					t.Fatalf("conditions = %+v, want %+v", c.Conditions, tt.want)
				}
			}
		})
	}
}

func TestExpireConditionsDoesNotWriteSharedBacking(t *testing.T) {
	one := 2
	shared := []data.Condition{{Name: "A", RoundsRemaining: &one}}
	c := data.Combatant{Conditions: shared}
	expireConditions(&c)
	if *shared[0].RoundsRemaining != 2 {
		t.Fatalf("original condition mutated to %d", *shared[0].RoundsRemaining)
	}
}

func TestTurnPositions(t *testing.T) {
	tests := []struct {
		name        string
		fn          func(pos, n int) (int, bool)
		pos, n      int
		wantIdx     int
		wantWrapped bool
	}{
		{"next middle", nextTurn, 0, 3, 1, false},
		{"next wraps at the end", nextTurn, 2, 3, 0, true},
		{"next from no current is first, no wrap", nextTurn, -1, 3, 0, false},
		{"next with single combatant wraps", nextTurn, 0, 1, 0, true},
		{"prev middle", prevTurn, 2, 3, 1, false},
		{"prev wraps at the start", prevTurn, 0, 3, 2, true},
		{"prev from no current wraps to last", prevTurn, -1, 3, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, wrapped := tt.fn(tt.pos, tt.n)
			if idx != tt.wantIdx || wrapped != tt.wantWrapped {
				t.Fatalf("got (%d, %v), want (%d, %v)", idx, wrapped, tt.wantIdx, tt.wantWrapped)
			}
		})
	}
}
