package data

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// maxScrubRetries bounds how often a scrub re-reads an encounter that changed under it.
const maxScrubRetries = 3

// sessionEncounterLimit bounds how many of a session's encounters one scrub reads.
const sessionEncounterLimit = 500

// UpdateInSession writes set to the session's encounter id if its revision is still
// expectedRevision, bumping the revision and stamping updated_*. d must grant can_edit (a GM or
// co-GM); the match is one atomic write on {_id, session_id, revision}, so another session's
// encounter, a private one, and a stale revision all modify nothing. Failures are classified like
// Owned.Update: ErrNotFound, ErrLocked, ErrInactive, or ErrRevisionConflict (with the current
// record returned).
//
// Only "combatants" may be set today: it is the field session edits (controller assignment,
// approved change requests) change. Each combatant is validated and ref_keys is recomputed.
func (r *Encounters) UpdateInSession(ctx context.Context, d Decision, id string, expectedRevision int64, set map[string]any) (Encounter, error) {
	var out Encounter
	if err := d.require(relEdit); err != nil {
		return out, err
	}
	id, ok := cleanID(id)
	if !ok {
		return out, ErrNotFound
	}
	cs, ok := set["combatants"].([]Combatant)
	if !ok || len(set) != 1 {
		return out, fmt.Errorf("%w: a session write sets combatants only", ErrInvalid)
	}
	if len(cs) > MaxCombatants {
		return out, fmt.Errorf("%w: more than %d combatants", ErrInvalid, MaxCombatants)
	}
	for _, c := range cs {
		if err := validateCombatant(c); err != nil {
			return out, err
		}
	}
	filter := bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil},
		{Key: "revision", Value: expectedRevision}, {Key: "inactive_at", Value: nil}, {Key: "locked_at", Value: nil}}
	update := bson.D{
		{Key: "$set", Value: bson.D{{Key: "combatants", Value: nonNilC(cs)}, {Key: "ref_keys", Value: nonNil(refKeys(cs))},
			{Key: "updated_at", Value: r.now()}, {Key: "updated_by", Value: d.actor}}},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	err := r.c.FindOneAndUpdate(ctx, filter, update, &out)
	if errors.Is(err, store.ErrNotFound) {
		return r.classifyInSession(ctx, d, id)
	}
	return out, err
}

// classifyInSession explains why a session write matched nothing.
func (r *Encounters) classifyInSession(ctx context.Context, d Decision, id string) (Encounter, error) {
	var cur Encounter
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}}, &cur)
	if errors.Is(err, store.ErrNotFound) {
		return Encounter{}, ErrNotFound
	}
	if err != nil {
		return Encounter{}, err
	}
	switch {
	case cur.LockedAt != nil:
		return Encounter{}, ErrLocked
	case cur.InactiveAt != nil:
		return Encounter{}, ErrInactive
	}
	return cur, ErrRevisionConflict
}

// ScrubController removes userID from every combatant's controller_user_ids in the session's
// encounters. It keeps a user who left, was removed, blocked, or demoted from regaining control
// of a combatant by rejoining or being promoted, and keeps the GM's view free of stale ids. d
// grants can_edit, or can_view when the user is scrubbing their own leave. Each changed encounter
// is written under its revision (the revision advances, so clients re-read); an encounter that
// changed under the scrub is re-read, up to maxScrubRetries times. Idempotent.
func (r *Encounters) ScrubController(ctx context.Context, d Decision, userID string) error {
	need := relEdit
	if userID == d.actor {
		need = relView
	}
	if err := pairOK(d, need, userID); err != nil {
		return err
	}
	var encs []Encounter
	if err := r.c.Find(ctx, bson.D{{Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}},
		store.FindOptions{Limit: sessionEncounterLimit}, &encs); err != nil {
		return err
	}
	var errs []error
	for _, e := range encs {
		if err := r.scrubOne(ctx, d, e, userID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Encounters) scrubOne(ctx context.Context, d Decision, e Encounter, userID string) error {
	for range maxScrubRetries {
		cs, changed := withoutController(e.Combatants, userID)
		if !changed {
			return nil
		}
		filter := bson.D{{Key: "_id", Value: e.ID}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}, {Key: "revision", Value: e.Revision}}
		update := bson.D{
			{Key: "$set", Value: bson.D{{Key: "combatants", Value: cs}, {Key: "updated_at", Value: r.now()}, {Key: "updated_by", Value: d.actor}}},
			{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
		}
		var discard Encounter
		err := r.c.FindOneAndUpdate(ctx, filter, update, &discard)
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// The encounter changed or was deleted since it was read: re-read and try again.
		if err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: e.ID}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}}, &e); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
	}
	return fmt.Errorf("encounter %s kept changing while scrubbing a controller", e.ID)
}

// withoutController returns cs with userID removed from every controller list, and whether
// anything changed. The input is not modified.
func withoutController(cs []Combatant, userID string) ([]Combatant, bool) {
	out := slices.Clone(cs)
	changed := false
	for i := range out {
		if !slices.Contains(out[i].ControllerUserIDs, userID) {
			continue
		}
		changed = true
		out[i].ControllerUserIDs = slices.DeleteFunc(slices.Clone(out[i].ControllerUserIDs), func(u string) bool { return u == userID })
	}
	return out, changed
}
