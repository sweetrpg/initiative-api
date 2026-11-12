package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

var (
	orderings   = []string{OrderHighToLow, OrderLowToHigh, OrderPCVsAdversary, OrderPlayerManaged}
	tieBreakers = []string{TieRandom, TieAlphabetical, TieQuery}
	statuses    = []string{StatusDraft, StatusActive, StatusEnded}
)

// Encounters is the owner-scoped encounter repository. It validates enumerated values and
// queue references before any write reaches the collection.
type Encounters struct {
	*Owned[Encounter, *Encounter]
	systems *SystemRepo
}

// NewEncounters builds the encounter repository for db.
func NewEncounters(db store.Database) *Encounters {
	return &Encounters{Owned: NewOwned[Encounter](db, EncounterSpec), systems: NewSystemRepo(db)}
}

// ValidateEncounter checks a whole encounter document: ordering, tie breaker, and status come
// from their constants, and every turn_queue entry names one of the encounter's combatants.
func ValidateEncounter(e *Encounter) error {
	if !slices.Contains(orderings, e.Ordering) {
		return fmt.Errorf("%w: ordering %q", ErrInvalid, e.Ordering)
	}
	if !slices.Contains(tieBreakers, e.TieBreaker) {
		return fmt.Errorf("%w: tie_breaker %q", ErrInvalid, e.TieBreaker)
	}
	return ValidateState(EncounterState{Status: e.Status, Round: e.Round,
		CurrentCombatantID: e.CurrentCombatantID, TurnQueue: e.TurnQueue, Combatants: e.Combatants})
}

// EncounterState is the live combat state of an encounter, written as one unit.
type EncounterState struct {
	Status             string
	Round              int
	CurrentCombatantID string
	TurnQueue          []string
	Combatants         []Combatant
}

// ValidateState checks status, a non-negative round, unique combatant IDs, and that the queue and
// the current combatant refer to combatants of this state.
func ValidateState(st EncounterState) error {
	if !slices.Contains(statuses, st.Status) {
		return fmt.Errorf("%w: status %q", ErrInvalid, st.Status)
	}
	if st.Round < 0 {
		return fmt.Errorf("%w: round %d", ErrInvalid, st.Round)
	}
	ids := make(map[string]bool, len(st.Combatants))
	for _, c := range st.Combatants {
		if err := validateCombatant(c); err != nil {
			return err
		}
		if ids[c.ID] {
			return fmt.Errorf("%w: duplicate combatant id %q", ErrInvalid, c.ID)
		}
		ids[c.ID] = true
	}
	if st.CurrentCombatantID != "" && !ids[st.CurrentCombatantID] {
		return fmt.Errorf("%w: current combatant %q is not a combatant", ErrInvalid, st.CurrentCombatantID)
	}
	for _, id := range st.TurnQueue {
		if !ids[id] {
			return fmt.Errorf("%w: turn_queue entry %q is not a combatant", ErrInvalid, id)
		}
	}
	return nil
}

func validateEncounterSet(set map[string]any) error {
	if v, ok := set["game_system_id"]; ok {
		if s, isStr := v.(string); !isStr || strings.TrimSpace(s) == "" {
			return fmt.Errorf("%w: game_system_id must be a non-empty string", ErrInvalid)
		}
	}
	if v, ok := set["ordering"]; ok {
		if s, isStr := v.(string); !isStr || !slices.Contains(orderings, s) {
			return fmt.Errorf("%w: ordering %v", ErrInvalid, v)
		}
	}
	if v, ok := set["tie_breaker"]; ok {
		if s, isStr := v.(string); !isStr || !slices.Contains(tieBreakers, s) {
			return fmt.Errorf("%w: tie_breaker %v", ErrInvalid, v)
		}
	}
	return nil
}

// Create resets e's combat state to a fresh draft, validates it, then stores it as owner's new
// encounter. Callers cannot create an encounter that is already running.
func (r *Encounters) Create(ctx context.Context, ownerUserID, actingUserID string, e *Encounter) error {
	e.Status, e.Round, e.CurrentCombatantID, e.TurnQueue = StatusDraft, 0, "", nil
	if err := ValidateEncounter(e); err != nil {
		return err
	}
	if err := r.checkHealth(ctx, e.GameSystemID, e.Combatants, nil); err != nil {
		return err
	}
	e.RefKeys = refKeys(e.Combatants)
	return r.Owned.Create(ctx, ownerUserID, actingUserID, e)
}

// Update validates the enumerated values in set, then applies it with the revision check. A change
// of game system also checks every combatant's health against the new system.
func (r *Encounters) Update(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (Encounter, error) {
	if err := validateEncounterSet(set); err != nil {
		return Encounter{}, err
	}
	if sysID, ok := set["game_system_id"].(string); ok {
		if err := r.requireSystem(ctx, sysID); err != nil {
			return Encounter{}, err
		}
		// Every combatant's health must fit the new system before the switch is stored.
		cur, err := r.Load(ctx, ownerUserID, id)
		if err != nil {
			return Encounter{}, err
		}
		if err := r.checkHealth(ctx, sysID, cur.Combatants, nil); err != nil {
			return Encounter{}, err
		}
	}
	return r.Owned.Update(ctx, ownerUserID, actingUserID, id, expectedRevision, set)
}

func validateCombatant(c Combatant) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: combatant %q has no name", ErrInvalid, c.ID)
	}
	if !slices.Contains(Kinds, c.Kind) {
		return fmt.Errorf("%w: combatant %q kind %q", ErrInvalid, c.ID, c.Kind)
	}
	for _, cond := range c.Conditions {
		if strings.TrimSpace(cond.Name) == "" || (cond.RoundsRemaining != nil && *cond.RoundsRemaining < 1) {
			return fmt.Errorf("%w: combatant %q has an invalid condition", ErrInvalid, c.ID)
		}
	}
	return nil
}

// requireSystem rejects a game system that has no definition. The No System fallback applies only
// to an encounter whose existing system lost its definition, never to choosing one: creating an
// encounter and switching its system both require the definition to exist.
func (r *Encounters) requireSystem(ctx context.Context, sysID string) error {
	_, err := r.systems.Get(ctx, sysID)
	if errors.Is(err, ErrNotFound) {
		return invalid("the game system is not defined")
	}
	return err
}

// checkHealth validates the health of each combatant in cs against the encounter's game system
// definition (the default system when the encounter's has none). Only combatants that are new or
// whose health differs from existing are checked, so a stored value that a later definition change
// made invalid does not block unrelated edits. The system is looked up only when something needs
// checking.
func (r *Encounters) checkHealth(ctx context.Context, sysID string, cs, existing []Combatant) error {
	before := make(map[string]map[string]int, len(existing))
	for _, c := range existing {
		before[c.ID] = c.Health
	}
	var changed []Combatant
	for _, c := range cs {
		if prev, ok := before[c.ID]; len(c.Health) > 0 && (!ok || !maps.Equal(prev, c.Health)) {
			changed = append(changed, c)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sys, err := r.systems.GetOrDefault(ctx, sysID)
	if errors.Is(err, ErrNotFound) {
		return invalid("the encounter's game system has no definition and no default system is defined")
	}
	if err != nil {
		return err
	}
	for _, c := range changed {
		if err := CheckHealth(sys, c.Health); err != nil {
			return err
		}
	}
	return nil
}

// SetState validates st and writes it as the encounter's combat state with the revision check.
func (r *Encounters) SetState(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, st EncounterState) (Encounter, error) {
	return r.Write(ctx, ownerUserID, actingUserID, id, expectedRevision, nil, st)
}

// Write validates settings (Updatable fields) and st (structure, and combatant health against the
// encounter's game system), then writes both in one atomic write with the revision check. Use it
// when a setting change, such as the ordering, comes with recomputed state.
func (r *Encounters) Write(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, settings map[string]any, st EncounterState) (Encounter, error) {
	if err := validateEncounterSet(settings); err != nil {
		return Encounter{}, err
	}
	if err := ValidateState(st); err != nil {
		return Encounter{}, err
	}
	if sysID, ok := settings["game_system_id"].(string); ok {
		if err := r.requireSystem(ctx, sysID); err != nil {
			return Encounter{}, err
		}
	}
	if slices.ContainsFunc(st.Combatants, func(c Combatant) bool { return len(c.Health) > 0 }) {
		cur, err := r.Load(ctx, ownerUserID, id)
		if err != nil {
			return Encounter{}, err
		}
		sysID := cur.GameSystemID
		if v, ok := settings["game_system_id"].(string); ok {
			sysID = v
		}
		existing := cur.Combatants
		if _, switched := settings["game_system_id"]; switched {
			existing = nil // a new system: every combatant's health is checked afresh
		}
		if err := r.checkHealth(ctx, sysID, st.Combatants, existing); err != nil {
			return Encounter{}, err
		}
	}
	state := map[string]any{
		"status": st.Status, "round": st.Round, "current_combatant_id": st.CurrentCombatantID,
		"turn_queue": nonNil(st.TurnQueue), "combatants": nonNilC(st.Combatants),
		"ref_keys": nonNil(refKeys(st.Combatants)),
	}
	return r.updateBoth(ctx, ownerUserID, actingUserID, id, expectedRevision, settings, state)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilC(s []Combatant) []Combatant {
	if s == nil {
		return []Combatant{}
	}
	return s
}

// Load reads owner's encounter; a miss is classified 403 (another owner's) or 404 (absent).
func (r *Encounters) Load(ctx context.Context, owner, id string) (Encounter, error) {
	cur, err := r.Get(ctx, owner, id)
	if errors.Is(err, ErrNotFound) {
		if cerr := r.Missing(ctx, owner, id); cerr != nil {
			return Encounter{}, cerr
		}
	}
	return cur, err
}

const (
	maxRefLen     = 200
	maxRefMatches = 100
)

// RefKey is the fixed-shape lookup key for one external reference: the hex SHA-256 of the
// length-prefixed key and value. Lookups filter on this alone, so request text never becomes part
// of a query filter.
func RefKey(key, value string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(key), key, value)))
	return hex.EncodeToString(sum[:])
}

// refKeys derives Encounter.RefKeys from the live (not removed) combatants.
func refKeys(cs []Combatant) []string {
	var out []string
	for _, c := range cs {
		if c.Removed {
			continue
		}
		for k, v := range c.ExternalRefs {
			out = append(out, RefKey(k, v))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// RefMatch is a combatant whose external_refs hold a looked-up pair, with its encounter's ID.
type RefMatch struct {
	EncounterID string    `json:"encounter_id"`
	Combatant   Combatant `json:"combatant"`
}

// FindByExternalRef returns owner's live combatants whose external_refs map key to value. The
// database matches on the derived RefKey; the stored values are then compared exactly, so a hash
// collision cannot return a wrong combatant.
func (r *Encounters) FindByExternalRef(ctx context.Context, owner, key, value string) ([]RefMatch, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrInvalid)
	}
	if key == "" || value == "" || len(key) > maxRefLen || len(value) > maxRefLen {
		return nil, invalid("ref_key and ref_value are required, at most %d bytes", maxRefLen)
	}
	filter := append(scope(owner), bson.E{Key: "ref_keys", Value: RefKey(key, value)})
	var encs []Encounter
	if err := r.c.Find(ctx, filter, store.FindOptions{Limit: maxRefMatches}, &encs); err != nil {
		return nil, err
	}
	var out []RefMatch
	for _, e := range encs {
		for _, c := range e.Combatants {
			if !c.Removed && c.ExternalRefs[key] == value {
				out = append(out, RefMatch{EncounterID: e.ID, Combatant: c})
			}
		}
	}
	return out, nil
}

// LockGated locks owner's encounters that use a game system other than No System, and returns how
// many it locked. A locked encounter keeps all its data; only writes stop (spec
// initiative-game-systems, "Losing the entitlement locks gated-system items"). Each pass takes one
// encounter, so a retry after a failure finishes the rest.
func (r *Encounters) LockGated(ctx context.Context, owner string) (int, error) {
	filter := append(scope(owner),
		bson.E{Key: "game_system_id", Value: bson.D{{Key: "$ne", Value: DefaultSystemID}}},
		bson.E{Key: "locked_at", Value: nil})
	return r.flip(ctx, filter, bson.D{{Key: "$set", Value: bson.D{{Key: "locked_at", Value: r.now()}}}})
}

// UnlockAll restores owner's locked encounters exactly as they were.
func (r *Encounters) UnlockAll(ctx context.Context, owner string) (int, error) {
	filter := append(scope(owner), bson.E{Key: "locked_at", Value: bson.D{{Key: "$ne", Value: nil}}})
	return r.flip(ctx, filter, bson.D{{Key: "$unset", Value: bson.D{{Key: "locked_at", Value: ""}}}})
}

func (r *Encounters) flip(ctx context.Context, filter, update bson.D) (int, error) {
	n := 0
	for {
		var out Encounter
		err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
		if errors.Is(err, store.ErrNotFound) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}
