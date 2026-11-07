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
	Name          string
	Filterable    []string
	Sortable      []string
	Projectable   []string
	UnsetOnDelete []string
	Indexes       []store.IndexSpec
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
		Indexes: []store.IndexSpec{
			{Name: "owner_name", Keys: asc("owner_user_id", "name")},
			{Name: "owner_updated", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "updated_at", Value: -1}}},
		},
	}

	// ThemeSpec: slot numbers are unique per owner among live themes. Soft delete unsets slot, and
	// the partial filter ($exists: true is the supported form) leaves slot-less rows out.
	ThemeSpec = Spec{
		Name:          CollectionThemes,
		Filterable:    []string{"name", "slot"},
		Sortable:      []string{"name", "slot", "created_at", "updated_at"},
		Projectable:   []string{"name", "style", "slot", "main_color", "accent1_color", "accent2_color", "background1_color", "background2_color", "primary_font_name", "secondary_font_name", "background_image", "revision", "owner_user_id", "created_at", "updated_at"},
		UnsetOnDelete: []string{"slot"},
		Indexes: []store.IndexSpec{
			{Name: "owner_slot", Keys: asc("owner_user_id", "slot"), Unique: true,
				Partial: bson.D{{Key: "slot", Value: bson.D{{Key: "$exists", Value: true}}}}},
		},
	}

	// CredentialSpec: secret_hash is unique so a presented secret resolves to one credential.
	CredentialSpec = Spec{
		Name:        CollectionCredentials,
		Filterable:  []string{"label"},
		Sortable:    []string{"label", "created_at", "last_used_at"},
		Projectable: []string{"label", "scopes", "revoked_at", "last_used_at", "revision", "owner_user_id", "created_at", "updated_at"},
		Indexes: []store.IndexSpec{
			{Name: "owner_created", Keys: bson.D{{Key: "owner_user_id", Value: 1}, {Key: "created_at", Value: -1}}},
			{Name: "secret_hash", Keys: asc("secret_hash"), Unique: true},
		},
	}

	// SystemSpec: aliases (including Foundry system IDs) are unique across systems.
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
func AllSpecs() []Spec { return []Spec{RosterSpec, ThemeSpec, CredentialSpec, SystemSpec} }

// EnsureIndexes creates every collection's indexes. Safe to run at each startup.
func EnsureIndexes(ctx context.Context, db store.Database) error {
	for _, s := range AllSpecs() {
		if err := db.Collection(s.Name).EnsureIndexes(ctx, s.Indexes); err != nil {
			return fmt.Errorf("ensure indexes on %s: %w", s.Name, err)
		}
	}
	return nil
}
