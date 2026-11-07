package data

import (
	"time"

	"github.com/sweetrpg/model-core.go/models"
)

// Collection names.
const (
	CollectionRosters     = "rosters"
	CollectionThemes      = "themes"
	CollectionGameSystems = "game_systems"
	CollectionCredentials = "integration_credentials"
)

// Character kinds.
const (
	KindPC        = "pc"
	KindAdversary = "adversary"
	KindObject    = "object"
)

// Credential scopes.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// Character is a reusable entry in a roster. Health holds default values keyed by the field keys
// of the game system's tracker definition.
type Character struct {
	ID           string            `bson:"id" json:"id"`
	Name         string            `bson:"name" json:"name"`
	Kind         string            `bson:"kind" json:"kind"`
	Notes        string            `bson:"notes" json:"notes"`
	Health       map[string]int    `bson:"health,omitempty" json:"health,omitempty"`
	ExternalRefs map[string]string `bson:"external_refs,omitempty" json:"external_refs,omitempty"`
}

// Roster is a reusable cast. Characters are embedded: a roster is one document and one write.
// Soft-deleted (PADR-0027: user-owned domain data).
type Roster struct {
	Meta       `bson:",inline"`
	Name       string      `bson:"name" json:"name"`
	Favorite   bool        `bson:"favorite" json:"favorite"`
	Characters []Character `bson:"characters" json:"characters"`
}

// Theme is a user's custom theme. Slot is the numbered editable slot it occupies; soft delete
// unsets it so the slot is free again (see ThemeSpec).
type Theme struct {
	Meta              `bson:",inline"`
	Name              string `bson:"name" json:"name"`
	Style             string `bson:"style" json:"style"`
	MainColor         string `bson:"main_color" json:"main_color"`
	Accent1Color      string `bson:"accent1_color" json:"accent1_color"`
	Accent2Color      string `bson:"accent2_color" json:"accent2_color"`
	Background1Color  string `bson:"background1_color" json:"background1_color"`
	Background2Color  string `bson:"background2_color" json:"background2_color"`
	PrimaryFontName   string `bson:"primary_font_name" json:"primary_font_name"`
	SecondaryFontName string `bson:"secondary_font_name" json:"secondary_font_name"`
	BackgroundImage   string `bson:"background_image,omitempty" json:"background_image,omitempty"`
	Slot              *int   `bson:"slot,omitempty" json:"slot,omitempty"`
}

// IntegrationCredential authenticates an external integration as its owner. Only the hash of the
// secret is stored; it never appears in a response. Revocation is a state change, not a delete.
type IntegrationCredential struct {
	Meta       `bson:",inline"`
	Label      string     `bson:"label" json:"label"`
	Scopes     []string   `bson:"scopes" json:"scopes"`
	SecretHash string     `bson:"secret_hash" json:"-"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
}

// Tracker field kinds.
const (
	FieldCounter = "counter"
	FieldPool    = "pool"
	FieldFlag    = "flag"
	FieldTrack   = "track"
)

// TrackerField is one health value in a game system's tracker definition. Max is a literal bound;
// MaxRef names another field in the same definition whose current value is the bound. At most one
// of the two is set.
type TrackerField struct {
	Key    string `bson:"key" json:"key"`
	Label  string `bson:"label" json:"label"`
	Kind   string `bson:"kind" json:"kind"`
	Def    int    `bson:"default" json:"default"`
	Min    *int   `bson:"min,omitempty" json:"min,omitempty"`
	Max    *int   `bson:"max,omitempty" json:"max,omitempty"`
	MaxRef string `bson:"max_ref,omitempty" json:"max_ref,omitempty"`
}

// GameSystem is API data, shared by all users (no owner). It is written only by the seed loader,
// as the system actor. ID is a stable slug. DefinitionHash lets the loader skip unchanged
// definitions.
type GameSystem struct {
	ID               string         `bson:"_id" json:"id"`
	Name             string         `bson:"name" json:"name"`
	Family           string         `bson:"family" json:"family"`
	Aliases          []string       `bson:"aliases" json:"aliases"`
	Gated            bool           `bson:"gated" json:"gated"`
	Fields           []TrackerField `bson:"fields" json:"fields"`
	DefinitionHash   string         `bson:"definition_hash" json:"-"`
	Revision         int64          `bson:"revision" json:"revision"`
	models.Auditable `bson:",inline"`
}
