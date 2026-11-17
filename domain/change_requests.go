package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"unicode"

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

// maxRejectReason bounds a rejection reason, in runes.
const maxRejectReason = 500

// ChangeInput is what a player proposes: health values to set, and/or conditions to add (or
// update by name) and remove by name.
type ChangeInput struct {
	Health           map[string]int
	AddConditions    []data.Condition
	RemoveConditions []string
}

func (in ChangeInput) empty() bool {
	return len(in.Health) == 0 && len(in.AddConditions) == 0 && len(in.RemoveConditions) == 0
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
// request can never alter initiative, notes, visibility, or controllers. Health values are set;
// conditions change by name against c as it is now: removals first, then each added condition
// replaces the one with its name or is appended. Applying it twice gives the same result.
func applyChange(c data.Combatant, in ChangeInput) data.Combatant {
	if len(in.Health) > 0 {
		merged := maps.Clone(c.Health)
		if merged == nil {
			merged = map[string]int{}
		}
		maps.Copy(merged, in.Health)
		c.Health = merged
	}
	if len(in.AddConditions) > 0 || len(in.RemoveConditions) > 0 {
		conds := slices.DeleteFunc(slices.Clone(c.Conditions), func(x data.Condition) bool {
			return slices.Contains(in.RemoveConditions, x.Name)
		})
		for _, add := range in.AddConditions {
			if i := slices.IndexFunc(conds, func(x data.Condition) bool { return x.Name == add.Name }); i >= 0 {
				conds[i] = add
			} else {
				conds = append(conds, add)
			}
		}
		if conds == nil {
			conds = []data.Condition{}
		}
		c.Conditions = conds
	}
	return c
}

// checkInput validates the shape of a proposal before any encounter is read.
func checkInput(in ChangeInput) error {
	if in.empty() {
		return invalid("a change request needs health values or conditions")
	}
	if len(in.AddConditions) > data.MaxConditions || len(in.RemoveConditions) > data.MaxConditions {
		return invalid("at most %d conditions can be added or removed in one request", data.MaxConditions)
	}
	for _, name := range in.RemoveConditions {
		if strings.TrimSpace(name) == "" {
			return invalid("a condition to remove needs a name")
		}
		if err := data.CheckText("condition name", name, data.MaxNameLen); err != nil {
			return err
		}
	}
	return nil
}

// Submit records the player's change request for a combatant they control. The caller must hold
// a live player Viewer on the session and be listed on the combatant now: an observer, a GM, a
// removed member, and a stale or demoted controller are all refused. The change is validated
// against the game system at submission as well as at approval.
func (s *ChangeRequests) Submit(ctx context.Context, actor, sessionID, encounterID, combatantID string, in ChangeInput) (data.ChangeRequest, error) {
	if err := checkInput(in); err != nil {
		return data.ChangeRequest{}, err
	}
	if sess, _, _, err := s.sessions.as(ctx, actor, sessionID, ActView); err != nil {
		return data.ChangeRequest{}, err
	} else if sess.InactiveAt != nil {
		return data.ChangeRequest{}, data.ErrInactive
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
	if err := s.encounter.repo.ValidateCombatantChange(ctx, enc.GameSystemID, c, applyChange(c, in)); err != nil {
		return data.ChangeRequest{}, err
	}
	req, err := s.repo.Create(ctx, v, data.ChangeRequest{
		EncounterID: enc.ID, CombatantID: c.ID, Health: in.Health, AddConditions: in.AddConditions,
		RemoveConditions: in.RemoveConditions, BaseRevision: enc.Revision,
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
//
// The request moves pending -> approving -> approved. The claim is taken before the write, so a
// concurrent reject or second approval loses; if the write fails the claim is undone. A claim that
// is never finished (a crash) is younger than ClaimTimeout: 409, retry later; older: any GM's
// approval resumes it, which is safe because the change is idempotent.
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
	next, enc, err := s.build(ctx, viewer, d, req)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if req, err = s.repo.Claim(ctx, d, encounterID, requestID); err != nil {
		return data.ChangeRequest{}, err
	}
	applied, err := s.write(ctx, viewer, d, req, next, enc)
	// Settling the claim must survive the caller going away: a request cancelled after the
	// encounter write would otherwise stay approving, and a later approval would re-apply values
	// that may be stale by then.
	settle := context.WithoutCancel(ctx)
	if err != nil {
		if uerr := s.repo.Unclaim(settle, d, encounterID, req.ID); uerr != nil {
			slog.ErrorContext(ctx, "could not release a change request claim after a failed approval; a GM can resume it once the claim is older than the timeout",
				"request", req.ID, "session", sessionID, "encounter", encounterID, "error", uerr)
		}
		return data.ChangeRequest{}, err
	}
	done, err := s.repo.Finish(settle, d, encounterID, req.ID, applied.Revision)
	if err != nil {
		// The change is applied but the request was not recorded as approved: another GM resumed
		// it after the timeout, it was cancelled, or the store failed. A resume re-applies the
		// idempotent change, so report what happened.
		slog.WarnContext(ctx, "change applied but the request was not recorded as approved", "request", req.ID,
			"session", sessionID, "encounter", encounterID, "applied_revision", applied.Revision, "error", err)
		req.Status, req.AppliedRevision = data.ChangeApproved, applied.Revision
		done = req
	}
	if s.Notifier != nil {
		s.Notifier.ChangeDecided(ctx, done)
	}
	return done, nil
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
	changed := applyChange(c, ChangeInput{Health: req.Health, AddConditions: req.AddConditions, RemoveConditions: req.RemoveConditions})
	if err := s.encounter.repo.ValidateCombatantChange(ctx, enc.GameSystemID, c, changed); err != nil {
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
	reason = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, reason))
	if reason == "" {
		return data.ChangeRequest{}, invalid("a reason is required to reject a change request")
	}
	if err := data.CheckText("reason", reason, maxRejectReason); err != nil {
		return data.ChangeRequest{}, err
	}
	_, _, d, err := s.sessions.as(ctx, actor, sessionID, ActManageMembers)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	req, err := s.repo.Reject(ctx, d, encounterID, requestID, reason)
	if err != nil {
		return data.ChangeRequest{}, err
	}
	if s.Notifier != nil {
		s.Notifier.ChangeDecided(ctx, req)
	}
	return req, nil
}
