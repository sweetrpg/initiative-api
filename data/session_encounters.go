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

// A scrub reads scrubPageSize encounters at a time and gives up after maxScrubPages pages.
const (
	scrubPageSize = 100
	maxScrubPages = 1000
)

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
// grants can_edit, or can_view when the user is scrubbing their own leave. Only encounters that
// list the user are read, a page at a time by _id until none is left, so no encounter is skipped;
// if the session has more than maxScrubPages pages the scrub fails rather than stop silently.
// Each changed encounter is written under its revision (the revision advances, so clients
// re-read) and re-read up to maxScrubRetries times if it changed under the scrub. Idempotent.
func (r *Encounters) ScrubController(ctx context.Context, d Decision, userID string) error {
	need := relEdit
	if userID == d.actor {
		need = relView
	}
	if err := pairOK(d, need, userID); err != nil {
		return err
	}
	var errs []error
	after := ""
	for page := 0; ; page++ {
		if page == maxScrubPages {
			return errors.Join(append(errs, fmt.Errorf("%w: more than %d encounters list this controller", ErrInvalid, maxScrubPages*scrubPageSize))...)
		}
		filter := bson.D{{Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil},
			{Key: "combatants.controller_user_ids", Value: userID}}
		if after != "" {
			filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: after}}})
		}
		var encs []Encounter
		if err := r.c.Find(ctx, filter, store.FindOptions{Sort: bson.D{{Key: "_id", Value: 1}}, Limit: scrubPageSize}, &encs); err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, e := range encs {
			if err := r.scrubOne(ctx, d, e, userID); err != nil {
				errs = append(errs, err)
			}
			after = e.ID
		}
		if len(encs) < scrubPageSize {
			return errors.Join(errs...)
		}
	}
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

// ReleaseSession turns every encounter of the session into a private encounter of its GM (the
// session's owner, d.actor): session_id and every combatant's controller_user_ids are cleared,
// the revision advances, and owner_user_id becomes the GM (initiative-sessions, "Session
// deletion"). d must be the GM's. It reads a page at a time by _id until none is left, and an
// encounter that changed under it is re-read, so no encounter is skipped; running it again after a
// partial failure finishes the rest (a released encounter no longer matches). Position and ref_keys
// are kept. The GM's saved-encounter limit reconciles afterwards like any other count change.
func (r *Encounters) ReleaseSession(ctx context.Context, d Decision) error {
	if err := d.require(relGM); err != nil {
		return err
	}
	var errs []error
	after := ""
	for page := 0; ; page++ {
		if page == maxScrubPages {
			return errors.Join(append(errs, fmt.Errorf("%w: more than %d encounters in one session", ErrInvalid, maxScrubPages*scrubPageSize))...)
		}
		filter := bson.D{{Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}}
		if after != "" {
			filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: after}}})
		}
		var encs []Encounter
		if err := r.c.Find(ctx, filter, store.FindOptions{Sort: bson.D{{Key: "_id", Value: 1}}, Limit: scrubPageSize}, &encs); err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, e := range encs {
			if err := r.releaseOne(ctx, d, e); err != nil {
				errs = append(errs, err)
			}
			after = e.ID
		}
		if len(encs) < scrubPageSize {
			return errors.Join(errs...)
		}
	}
}

func (r *Encounters) releaseOne(ctx context.Context, d Decision, e Encounter) error {
	for range maxScrubRetries {
		cs := slices.Clone(e.Combatants)
		for i := range cs {
			cs[i].ControllerUserIDs = nil
		}
		filter := bson.D{{Key: "_id", Value: e.ID}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}, {Key: "revision", Value: e.Revision}}
		update := bson.D{
			{Key: "$set", Value: bson.D{{Key: "combatants", Value: nonNilC(cs)}, {Key: "owner_user_id", Value: d.actor},
				{Key: "updated_at", Value: r.now()}, {Key: "updated_by", Value: d.actor}}},
			{Key: "$unset", Value: bson.D{{Key: "session_id", Value: ""}}},
			{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
		}
		var discard Encounter
		err := r.c.FindOneAndUpdate(ctx, filter, update, &discard)
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: e.ID}, {Key: "session_id", Value: d.session}, {Key: "deleted_at", Value: nil}}, &e); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
	}
	return fmt.Errorf("encounter %s kept changing while its session was being deleted", e.ID)
}

// ValidateCombatantChange checks after, a combatant as it would be once an edit applies to before
// (the combatant as stored), against the rules a write enforces: its own fields, and the health
// keys the edit changed against the game system's definition (the default system when the
// encounter's has none). A stored value that a later definition change made invalid, in a key the
// edit does not touch, does not block the edit; an edit that leaves health alone checks none.
func (r *Encounters) ValidateCombatantChange(ctx context.Context, gameSystemID string, before, after Combatant) error {
	if err := validateCombatant(after); err != nil {
		return err
	}
	var changed []string
	for k, v := range after.Health {
		if old, ok := before.Health[k]; !ok || old != v {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sys, err := r.systems.GetOrDefault(ctx, gameSystemID)
	if errors.Is(err, ErrNotFound) {
		return invalid("the encounter's game system has no definition and no default system is defined")
	}
	if err != nil {
		return err
	}
	slices.Sort(changed)
	return checkHealthKeys(sys, after.Health, changed)
}
