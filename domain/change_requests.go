package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
)

// ErrChangeStale means an approved change no longer applies: the combatant is gone, its
// requester no longer controls it, or the values no longer validate against the game system. The
// request stays pending.
var ErrChangeStale = errors.New("domain: the requested change no longer applies")

// maxApplyAttempts bounds how often an approval re-reads an encounter that changed under it.
const maxApplyAttempts = 3

// ChangeNotifier is the hook point for telling users about change requests (the GM and co-GMs
// when one is submitted, the requester when it is decided). Events arrive in a later change; an
// implementation must not block or fail the request, and the default does nothing.
type ChangeNotifier interface {
	ChangeSubmitted(ctx context.Context, req data.ChangeRequest)
	ChangeDecided(ctx context.Context, req data.ChangeRequest)
}

// ChangeInput is what a player proposes: health values to set, and/or a replacement conditions
// list (non-nil, possibly empty).
type ChangeInput struct {
	Health     map[string]int
	Conditions *[]data.Condition
}

// ChangeRequests implements change requests: submit, list, approve, and reject.
type ChangeRequests struct {
	repo      *data.ChangeRequests
	encounter *Encounters
	sessions  *Sessions
	// Notifier is told about submissions and decisions; nil means nobody is.
	Notifier ChangeNotifier
}

// NewChangeRequests builds the service on db.
func NewChangeRequests(db store.Database, encounters *Encounters, sessions *Sessions) *ChangeRequests {
	return &ChangeRequests{repo: data.NewChangeRequests(db), encounter: encounters, sessions: sessions}
}

// applyChange returns c with in applied. Only health and conditions can change here, so a change
// request can never alter initiative, notes, visibility, or controllers.
func applyChange(c data.Combatant, in ChangeInput) data.Combatant {
	if len(in.Health) > 0 {
		merged := maps.Clone(c.Health)
		if merged == nil {
			merged = map[string]int{}
		}
		maps.Copy(merged, in.Health)
		c.Health = merged
	}
	if in.Conditions != nil {
		c.Conditions = slices.Clone(*in.Conditions)
		if c.Conditions == nil {
			c.Conditions = []data.Condition{}
		}
	}
	return c
}

// Submit records the player's change request for a combatant they control. The caller must hold
// a live player Viewer on the session and be listed on the combatant now: an observer, a GM, a
// removed member, and a stale or demoted controller are all refused. The change is validated
// against the game system at submission as well as at approval.
func (s *ChangeRequests) Submit(ctx context.Context, actor, sessionID, encounterID, combatantID string, in ChangeInput) (data.ChangeRequest, error) {
	if len(in.Health) == 0 && in.Conditions == nil {
		return data.ChangeRequest{}, invalid("a change request needs health values or conditions")
	}
	v, err := s.sessions.Viewer(ctx, actor, sessionID)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if v.Level() != data.ViewPlayer {
		return data.ChangeRequest{}, data.ErrForbidden
	}
	enc, err := s.encounter.repo.LoadForViewer(ctx, v, encounterID)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	c, ok := findLive(enc, combatantID)
	if !ok || (c.Hidden && !controls(c, v)) {
		return data.ChangeRequest{}, data.ErrNotFound
	}
	if !controls(c, v) {
		return data.ChangeRequest{}, data.ErrForbidden
	}
	switch {
	case enc.LockedAt != nil:
		return data.ChangeRequest{}, data.ErrLocked
	case enc.InactiveAt != nil:
		return data.ChangeRequest{}, data.ErrInactive
	}
	if err := s.encounter.repo.ValidateCombatantChange(ctx, enc.GameSystemID, applyChange(c, in)); err != nil {
		return data.ChangeRequest{}, err
	}
	req, err := s.repo.Create(ctx, v, data.ChangeRequest{
		EncounterID: enc.ID, CombatantID: c.ID, Health: in.Health, Conditions: in.Conditions, BaseRevision: enc.Revision,
	})
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if s.Notifier != nil {
		s.Notifier.ChangeSubmitted(ctx, req)
	}
	return req, nil
}

func findLive(enc data.Encounter, combatantID string) (data.Combatant, bool) {
	i := slices.IndexFunc(enc.Combatants, func(c data.Combatant) bool { return c.ID == combatantID && !c.Removed })
	if i < 0 {
		return data.Combatant{}, false
	}
	return enc.Combatants[i], true
}

// List returns one page of the encounter's change requests: all of them for a GM or co-GM, the
// caller's own for a player, and an observer is refused. status narrows to one status.
func (s *ChangeRequests) List(ctx context.Context, actor, sessionID, encounterID, status string, q data.Query) (data.Page[data.ChangeRequest], error) {
	v, err := s.sessions.Viewer(ctx, actor, sessionID)
	if err != nil {
		return data.Page[data.ChangeRequest]{}, err
	}
	if _, err := s.encounter.repo.LoadForViewer(ctx, v, encounterID); err != nil {
		return data.Page[data.ChangeRequest]{}, err
	}
	return s.repo.List(ctx, v, encounterID, status, q)
}

// Approve applies a pending request through the same atomic, revision-bumping encounter write as
// any other edit, and records who approved it. The change is re-validated against the encounter
// as it is now, and the request's requester must still control the combatant as a player; a
// change that no longer applies is ErrChangeStale and the request stays pending. An encounter
// that moved on since the request was submitted is not a reason to fail: the request is applied
// to the current revision.
func (s *ChangeRequests) Approve(ctx context.Context, actor, sessionID, encounterID, requestID string) (data.ChangeRequest, error) {
	_, _, d, err := s.sessions.as(ctx, actor, sessionID, ActManageMembers)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	viewer, err := s.sessions.Viewer(ctx, actor, sessionID)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	req, err := s.repo.Load(ctx, d, encounterID, requestID)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if req.Status != data.ChangePending {
		return req, data.ErrConflict
	}
	next, enc, err := s.build(ctx, viewer, d, req)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	// Claim the request before the write, so a concurrent reject or second approval cannot also
	// win. Everything that can make the change stale was checked above, so a later failure is a
	// conflict or an outage, and the claim is undone.
	if req, err = s.repo.Decide(ctx, d, encounterID, requestID, data.ChangeApproved, ""); err != nil {
		return data.ChangeRequest{}, err
	}
	applied, err := s.write(ctx, viewer, d, req, next, enc)
	if err != nil {
		if rerr := s.repo.Reopen(ctx, d, encounterID, req.ID); rerr != nil {
			slog.ErrorContext(ctx, "could not reopen a change request after a failed approval", "request", req.ID, "error", rerr)
		}
		return data.ChangeRequest{}, err
	}
	if done, merr := s.repo.MarkApplied(ctx, d, encounterID, req.ID, applied.Revision); merr == nil {
		req = done
	} else {
		slog.WarnContext(ctx, "could not record the revision a change request produced", "request", req.ID, "error", merr)
		req.AppliedRevision = applied.Revision
	}
	if s.Notifier != nil {
		s.Notifier.ChangeDecided(ctx, req)
	}
	return req, nil
}

// write stores next as the encounter's combatants at enc's revision. If the encounter changed
// since it was read it is re-read and the change rebuilt and re-validated, up to
// maxApplyAttempts times.
func (s *ChangeRequests) write(ctx context.Context, v data.Viewer, d data.Decision, req data.ChangeRequest, next []data.Combatant, enc data.Encounter) (data.Encounter, error) {
	for attempt := 0; ; attempt++ {
		updated, err := s.encounter.repo.UpdateInSession(ctx, d, enc.ID, enc.Revision, map[string]any{"combatants": next})
		if !errors.Is(err, data.ErrRevisionConflict) || attempt+1 == maxApplyAttempts {
			return updated, err
		}
		if next, enc, err = s.build(ctx, v, d, req); err != nil {
			return data.Encounter{}, err
		}
	}
}

// build loads the encounter fresh and returns its combatants with req applied, and the encounter
// they were built from. Anything that stops the change applying is ErrChangeStale.
func (s *ChangeRequests) build(ctx context.Context, v data.Viewer, d data.Decision, req data.ChangeRequest) ([]data.Combatant, data.Encounter, error) {
	enc, err := s.encounter.repo.LoadForViewer(ctx, v, req.EncounterID)
	if err != nil {
		return nil, enc, err
	}
	i := slices.IndexFunc(enc.Combatants, func(c data.Combatant) bool { return c.ID == req.CombatantID && !c.Removed })
	if i < 0 {
		return nil, enc, fmt.Errorf("%w: the combatant is gone", ErrChangeStale)
	}
	c := enc.Combatants[i]
	if !slices.Contains(c.ControllerUserIDs, req.RequestedBy) {
		return nil, enc, fmt.Errorf("%w: the requester no longer controls the combatant", ErrChangeStale)
	}
	m, err := s.sessions.repo.Member(ctx, d, req.RequestedBy)
	switch {
	case errors.Is(err, data.ErrNotFound):
		return nil, enc, fmt.Errorf("%w: the requester has left the session", ErrChangeStale)
	case err != nil:
		return nil, enc, err
	case m.Role != data.RolePlayer:
		return nil, enc, fmt.Errorf("%w: the requester is no longer a player", ErrChangeStale)
	}
	changed := applyChange(c, ChangeInput{Health: req.Health, Conditions: req.Conditions})
	if err := s.encounter.repo.ValidateCombatantChange(ctx, enc.GameSystemID, changed); err != nil {
		if errors.Is(err, data.ErrInvalid) {
			return nil, enc, fmt.Errorf("%w: %s", ErrChangeStale, strings.TrimPrefix(err.Error(), data.ErrInvalid.Error()+": "))
		}
		return nil, enc, err
	}
	cs := slices.Clone(enc.Combatants)
	cs[i] = changed
	return cs, enc, nil
}

// Reject settles a pending request with a reason; the combatant is unchanged and the requester
// sees the reason.
func (s *ChangeRequests) Reject(ctx context.Context, actor, sessionID, encounterID, requestID, reason string) (data.ChangeRequest, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return data.ChangeRequest{}, invalid("a reason is required to reject a change request")
	}
	if err := data.CheckText("reason", reason, data.MaxNotesLen); err != nil {
		return data.ChangeRequest{}, err
	}
	_, _, d, err := s.sessions.as(ctx, actor, sessionID, ActManageMembers)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	req, err := s.repo.Decide(ctx, d, encounterID, requestID, data.ChangeRejected, reason)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if s.Notifier != nil {
		s.Notifier.ChangeDecided(ctx, req)
	}
	return req, nil
}
