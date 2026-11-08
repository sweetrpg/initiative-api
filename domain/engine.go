package domain

import (
	"cmp"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
)

// Pure turn-order logic: no I/O, no clock. Randomness comes in as a permutation function.

type entry struct {
	c       data.Combatant
	listIdx int
	prevIdx int // position in the previous queue, -1 if it was not queued
	rank    int // random rank among newly queued combatants
}

func kindGroup(ordering, kind string) int {
	if ordering != data.OrderPCVsAdversary {
		return 0
	}
	switch kind {
	case data.KindPC:
		return 0
	case data.KindAdversary:
		return 1
	}
	return 2
}

// initiativeCmp orders by initiative, unset last. lowToHigh ascends; everything else descends.
func initiativeCmp(ordering string, a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case ordering == data.OrderLowToHigh:
		return cmp.Compare(*a, *b)
	}
	return cmp.Compare(*b, *a)
}

func tieCmp(tie string, prevLen int, a, b entry) int {
	switch tie {
	case data.TieAlphabetical:
		if c := strings.Compare(strings.ToLower(a.c.Name), strings.ToLower(b.c.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.c.ID, b.c.ID)
	case data.TieRandom:
		// Combatants already queued keep their relative order, so a recompute never reshuffles
		// ties mid-combat. Newly queued ones fall in behind them in random order.
		return cmp.Compare(randKey(a, prevLen), randKey(b, prevLen))
	}
	return cmp.Compare(a.listIdx, b.listIdx) // data.TieQuery: encounter list order
}

func randKey(e entry, prevLen int) int {
	if e.prevIdx >= 0 {
		return e.prevIdx
	}
	return prevLen + e.rank
}

// buildQueue returns the IDs of the non-removed combatants in turn order. prev is the existing
// queue. perm(n) returns a random permutation of 0..n-1.
//
// playerManaged keeps prev's order, dropping departed combatants and appending new ones in list
// order. The other orderings sort by kind group (pcVsAdversary only: pc, adversary, object), then
// initiative, then the tie breaker.
func buildQueue(ordering, tie string, cs []data.Combatant, prev []string, perm func(int) []int) []string {
	prevIdx := make(map[string]int, len(prev))
	for i, id := range prev {
		prevIdx[id] = i
	}
	var es []entry
	for i, c := range cs {
		if c.Removed {
			continue
		}
		pi, ok := prevIdx[c.ID]
		if !ok {
			pi = -1
		}
		es = append(es, entry{c: c, listIdx: i, prevIdx: pi})
	}
	if ordering == data.OrderPlayerManaged {
		return manualQueue(es)
	}
	assignRanks(es, perm)
	slices.SortStableFunc(es, func(a, b entry) int {
		return cmp.Or(
			cmp.Compare(kindGroup(ordering, a.c.Kind), kindGroup(ordering, b.c.Kind)),
			initiativeCmp(ordering, a.c.Initiative, b.c.Initiative),
			tieCmp(tie, len(prev), a, b),
			cmp.Compare(a.listIdx, b.listIdx),
		)
	})
	return ids(es)
}

func assignRanks(es []entry, perm func(int) []int) {
	var fresh []int
	for i, e := range es {
		if e.prevIdx < 0 {
			fresh = append(fresh, i)
		}
	}
	if len(fresh) == 0 {
		return
	}
	for r, p := range perm(len(fresh)) {
		es[fresh[r]].rank = p
	}
}

func manualQueue(es []entry) []string {
	slices.SortStableFunc(es, func(a, b entry) int {
		switch {
		case a.prevIdx >= 0 && b.prevIdx >= 0:
			return cmp.Compare(a.prevIdx, b.prevIdx)
		case a.prevIdx >= 0:
			return -1
		case b.prevIdx >= 0:
			return 1
		}
		return cmp.Compare(a.listIdx, b.listIdx)
	})
	return ids(es)
}

func ids(es []entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.c.ID)
	}
	return out
}

// expireConditions applies the start-of-turn rule to c: each timed condition loses one round and
// is removed at zero. Open-ended conditions are untouched. It builds a new slice, so shared
// backing arrays are never written.
func expireConditions(c *data.Combatant) {
	next := make([]data.Condition, 0, len(c.Conditions))
	for _, cond := range c.Conditions {
		if cond.RoundsRemaining == nil {
			next = append(next, cond)
			continue
		}
		if left := *cond.RoundsRemaining - 1; left > 0 {
			next = append(next, data.Condition{Name: cond.Name, RoundsRemaining: &left})
		}
	}
	c.Conditions = next
}

// nextTurn returns the queue position after pos and whether it wrapped past the end. pos -1 (no
// current combatant) yields position 0 without wrapping.
func nextTurn(pos, n int) (idx int, wrapped bool) {
	if pos+1 >= n {
		return 0, pos >= 0
	}
	return pos + 1, false
}

// prevTurn returns the queue position before pos and whether it wrapped past the start.
func prevTurn(pos, n int) (idx int, wrapped bool) {
	if pos <= 0 {
		return n - 1, true
	}
	return pos - 1, false
}
