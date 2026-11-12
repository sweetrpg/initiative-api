package data

import (
	"context"
	"fmt"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// Spec declares one collection: its name, the fields a client may filter, sort, and project by
// (allow-lists; anything else is rejected, PADR-0030), fields to unset on soft delete, and its
// indexes.
type Spec struct {
	Name        string
	Filterable  []string
	Sortable    []string
	Projectable []string
	// Updatable lists the fields Owned.Update may set. State changes with their own rules
	// (credential revocation, last use, scopes, secret) are not here; they get dedicated functions.
	Updatable []string
	// StateFields lists the fields only Encounters.SetState may set: results of domain rules.
	StateFields   []string
	UnsetOnDelete []string
	// ConflictOnDuplicate maps a unique-index rejection to ErrConflict. Set it only where the
	// collection's one unique index is a user-facing constraint (theme slot); elsewhere the raw
	// store.ErrDuplicate is returned so it is not mislabeled.
	ConflictOnDuplicate bool
	Indexes             []store.IndexSpec
}

func asc(fields ...string) bson.D {
	d := make(bson.D, 0, len(fields))
	for _, f := range fields {
		d = append(d, bson.E{Key: f, Value: 1})
	}
	return d
}

// Specs for each collection. Every owner-scoped index leads with owner_user_id, because every
// query filters by it.
var (
	RosterSpec = Spec{
		Name:        CollectionRosters,
		Filterable:  []string{"name", "favorite"},
		Sortable:    []string{"name", "favorite", "created_at", "updated_at"},
		Projectable: []string{"name", "favorite", "characters", "revision", "owner_user_id", "created_at", "updated_at", "created_by", "updated_by"},
		Updatable:   []string{"name", "favorite", "characters"},
		Indexes: []store.IndexSpec{
			{Name: "owner_name", Keys: asc("owner_user_id", "name")},
			{Name: "owner_updated", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "updated_at", Value: -1}}},
		},
	}

	// EncounterSpec: the launcher lists an owner's encounters by position, optionally by status.
	// Combat state (status, round, current_combatant_id, turn_queue, combatants) is not
	// updatable here: it changes only through the domain functions that apply the tracking rules.
	EncounterSpec = Spec{
		Name:          CollectionEncounters,
		Filterable:    []string{"name", "status", "game_system_id", "position"},
		Sortable:      []string{"name", "status", "position", "created_at", "updated_at"},
		Projectable:   []string{"name", "game_system_id", "ordering", "tie_breaker", "labels", "colorblind", "theme_id", "status", "round", "current_combatant_id", "turn_queue", "position", "combatants", "revision", "owner_user_id", "created_at", "updated_at", "created_by", "updated_by"},
		Updatable:     []string{"name", "game_system_id", "ordering", "tie_breaker", "labels", "colorblind", "theme_id", "position"},
		StateFields:   []string{"status", "round", "current_combatant_id", "turn_queue", "combatants", "ref_keys"},
		UnsetOnDelete: []string{"ref_keys"},
		Indexes: []store.IndexSpec{
			{Name: "owner_position", Keys: asc("owner_user_id", "position")},
			{Name: "owner_ref_keys", Keys: asc("owner_user_id", "ref_keys")},
			{Name: "owner_name", Keys: asc("owner_user_id", "name")},
			{Name: "owner_created_at", Keys: asc("owner_user_id", "created_at")},
			{Name: "owner_updated_at", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "updated_at", Value: -1}}},
			{Name: "owner_status_updated", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}},
		},
	}

	// ThemeSpec: slot numbers are unique per owner among live themes. Soft delete unsets slot, and
	// the partial filter ($exists: true is the supported form) leaves slot-less rows out.
	ThemeSpec = Spec{
		Name:                CollectionThemes,
		Filterable:          []string{"name", "slot"},
		Sortable:            []string{"name", "slot", "created_at", "updated_at"},
		Projectable:         []string{"name", "style", "slot", "main_color", "accent1_color", "accent2_color", "background1_color", "background2_color", "primary_font_name", "secondary_font_name", "background_image", "revision", "owner_user_id", "created_at", "updated_at"},
		Updatable:           []string{"name", "style", "main_color", "accent1_color", "accent2_color", "background1_color", "background2_color", "primary_font_name", "secondary_font_name", "background_image", "slot"},
		UnsetOnDelete:       []string{"slot"},
		ConflictOnDuplicate: true,
		Indexes: []store.IndexSpec{
			{Name: "owner_slot", Keys: asc("owner_user_id", "slot"), Unique: true,
				Partial: bson.D{{Key: "slot", Value: bson.D{{Key: "$exists", Value: true}}}}},
		},
	}

	// CredentialSpec: secret_hash is unique so a presented secret resolves to one credential.
	CredentialSpec = Spec{
		Name:        CollectionCredentials,
		Filterable:  []string{"label"},
		Sortable:    []string{"label", "created_at"},
		Updatable:   []string{"label"},
		Projectable: []string{"label", "scopes", "revoked_at", "last_used_at", "revision", "owner_user_id", "created_at", "updated_at"},
		Indexes: []store.IndexSpec{
			{Name: "owner_created", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Name: "secret_hash", Keys: asc("secret_hash"), Unique: true},
		},
	}

	// SessionSpec: a session is owned by its GM, so owner-scoped create and soft delete apply to
	// the GM only. Reads by members go through Sessions.
	SessionSpec = Spec{
		Name:        CollectionSessions,
		Filterable:  []string{"name"},
		Sortable:    []string{"name", "created_at"},
		Projectable: []string{"name", "revision", "owner_user_id", "created_at", "updated_at", "created_by", "updated_by"},
		Updatable:   []string{"name"},
		Indexes:     []store.IndexSpec{{Name: "owner_created", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "created_at", Value: -1}}}},
	}

	// The membership collections are addressed by session_id, set from a record the caller was
	// already authorized for, never from request input alone.
	SessionMemberSpec = Spec{
		Name: CollectionSessionMembers, Sortable: []string{"created_at"},
		Indexes: []store.IndexSpec{
			{Name: "session_user", Keys: asc("session_id", "user_id"), Unique: true,
				Partial: bson.D{{Key: "live", Value: bson.D{{Key: "$exists", Value: true}}}}},
			{Name: "user", Keys: asc("user_id")},
		},
	}
	SessionInviteSpec = Spec{
		Name: CollectionSessionInvites, Sortable: []string{"created_at"},
		Indexes: []store.IndexSpec{
			{Name: "code_hash", Keys: asc("code_hash"), Unique: true},
			{Name: "session", Keys: asc("session_id")},
		},
	}
	SessionRequestSpec = Spec{
		Name: CollectionSessionRequests, Sortable: []string{"created_at"}, Filterable: []string{"status"},
		Indexes: []store.IndexSpec{{Name: "session_user", Keys: asc("session_id", "user_id"), Unique: true,
			Partial: bson.D{{Key: "live", Value: bson.D{{Key: "$exists", Value: true}}}}}},
	}
	SessionBlockSpec = Spec{
		Name: CollectionSessionBlocks, Sortable: []string{"created_at"},
		Indexes: []store.IndexSpec{{Name: "session_user", Keys: asc("session_id", "user_id"), Unique: true,
			Partial: bson.D{{Key: "live", Value: bson.D{{Key: "$exists", Value: true}}}}}},
	}

	// SystemSpec: aliases (including Foundry system IDs) are unique across systems.
	// SystemSpec has no Updatable fields: definitions change only through SystemRepo.Upsert.
	SystemSpec = Spec{
		Name:        CollectionGameSystems,
		Filterable:  []string{"family", "gated", "aliases", "name"},
		Sortable:    []string{"name", "family"},
		Projectable: []string{"name", "family", "aliases", "gated", "fields", "revision"},
		Indexes: []store.IndexSpec{
			{Name: "aliases", Keys: asc("aliases"), Unique: true},
			{Name: "family_name", Keys: asc("family", "name")},
		},
	}
)

// AllSpecs lists every collection the service owns.
func AllSpecs() []Spec {
	return []Spec{RosterSpec, EncounterSpec, ThemeSpec, CredentialSpec, SystemSpec,
		SessionSpec, SessionMemberSpec, SessionInviteSpec, SessionRequestSpec, SessionBlockSpec}
}

// EnsureIndexes creates every collection's indexes. Safe to run at each startup.
func EnsureIndexes(ctx context.Context, db store.Database) error {
	for _, s := range AllSpecs() {
		if err := db.Collection(s.Name).EnsureIndexes(ctx, s.Indexes); err != nil {
			return fmt.Errorf("ensure indexes on %s: %w", s.Name, err)
		}
	}
	return nil
}
