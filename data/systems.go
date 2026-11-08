package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/model-core.go/models"
	"go.mongodb.org/mongo-driver/bson"
)

// Outcome is what Upsert did with a definition.
type Outcome string

// Upsert outcomes.
const (
	Created   Outcome = "created"
	Updated   Outcome = "updated"
	Unchanged Outcome = "unchanged"
)

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SystemRepo reads and seeds game-system definitions. They are shared reference data with no
// owner, so there is no owner scoping; the only writer is the seed loader, as the system actor.
type SystemRepo struct {
	c   store.Collection
	now func() time.Time
}

// NewSystemRepo builds the repository for the game_systems collection in db.
func NewSystemRepo(db store.Database) *SystemRepo {
	return &SystemRepo{c: db.Collection(SystemSpec.Name), now: func() time.Time { return time.Now().UTC() }}
}

// Get returns the system whose ID or alias equals idOrAlias.
func (s *SystemRepo) Get(ctx context.Context, idOrAlias string) (GameSystem, error) {
	var out GameSystem
	filter := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "_id", Value: idOrAlias}}, bson.D{{Key: "aliases", Value: idOrAlias}},
	}}}
	err := s.c.FindOne(ctx, filter, &out)
	if errors.Is(err, store.ErrNotFound) {
		return out, ErrNotFound
	}
	return out, err
}

// DefaultSystemID is "No System": the simple tracker every user can use, and the definition an
// encounter falls back to when its game system has no definition.
const DefaultSystemID = "none"

// GetOrDefault returns the system for idOrAlias, or the default system when none is defined for it.
func (s *SystemRepo) GetOrDefault(ctx context.Context, idOrAlias string) (GameSystem, error) {
	sys, err := s.Get(ctx, idOrAlias)
	if errors.Is(err, ErrNotFound) {
		return s.Get(ctx, DefaultSystemID)
	}
	return sys, err
}

// List returns one page of systems; filtering (for example gated=false for users without the
// entitlement) goes through q.
func (s *SystemRepo) List(ctx context.Context, q Query) (Page[GameSystem], error) {
	return runQuery[GameSystem](ctx, s.c, SystemSpec, nil, q)
}

// Upsert creates or updates sys by ID. A definition identical to the stored one (same content
// hash) is left untouched, so re-seeding keeps IDs, revisions, and audit fields stable.
func (s *SystemRepo) Upsert(ctx context.Context, sys GameSystem) (Outcome, error) {
	if err := ValidateSystem(sys); err != nil {
		return "", err
	}
	hash, err := definitionHash(sys)
	if err != nil {
		return "", err
	}
	sys.DefinitionHash = hash

	// A concurrent loader can change the row between our read and write; re-read and retry once
	// before giving up, so a lost race is never reported as "unchanged".
	for attempt := 0; attempt < 2; attempt++ {
		var (
			cur GameSystem
			out Outcome
		)
		err = s.c.FindOne(ctx, bson.D{{Key: "_id", Value: sys.ID}}, &cur)
		switch {
		case errors.Is(err, store.ErrNotFound):
			out, err = s.create(ctx, sys)
		case err != nil:
			return "", err
		case cur.DefinitionHash == hash:
			return Unchanged, nil
		default:
			out, err = s.update(ctx, sys, cur.DefinitionHash)
		}
		if !errors.Is(err, errLostRace) {
			return out, err
		}
	}
	return "", fmt.Errorf("system %q: %w", sys.ID, errLostRace)
}

// errLostRace means another writer changed the definition between the read and the write.
var errLostRace = errors.New("data: concurrent change to system definition")

func (s *SystemRepo) create(ctx context.Context, sys GameSystem) (Outcome, error) {
	sys.Revision = 1
	models.StampCreate(&sys.Auditable, models.SystemActor, s.now())
	err := s.c.InsertOne(ctx, sys)
	if errors.Is(err, store.ErrDuplicate) {
		// Another replica may have inserted this same ID first; if so, retry as an update/no-op.
		var existing GameSystem
		switch rerr := s.c.FindOne(ctx, bson.D{{Key: "_id", Value: sys.ID}}, &existing); {
		case rerr == nil:
			return "", errLostRace
		case errors.Is(rerr, store.ErrNotFound):
			return "", fmt.Errorf("system %q alias conflicts with another system: %w", sys.ID, err)
		default:
			return "", rerr
		}
	}
	if err != nil {
		return "", err
	}
	return Created, nil
}

func (s *SystemRepo) update(ctx context.Context, sys GameSystem, prevHash string) (Outcome, error) {
	filter := bson.D{{Key: "_id", Value: sys.ID}, {Key: "definition_hash", Value: prevHash}}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "name", Value: sys.Name}, {Key: "family", Value: sys.Family},
			{Key: "aliases", Value: sys.Aliases}, {Key: "gated", Value: sys.Gated},
			{Key: "fields", Value: sys.Fields}, {Key: "definition_hash", Value: sys.DefinitionHash},
			{Key: "updated_at", Value: s.now()}, {Key: "updated_by", Value: models.SystemActor}}},
		{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}},
	}
	var out GameSystem
	err := s.c.FindOneAndUpdate(ctx, filter, update, &out)
	if errors.Is(err, store.ErrDuplicate) {
		return "", fmt.Errorf("system %q alias conflicts with another system: %w", sys.ID, err)
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", errLostRace
	}
	if err != nil {
		return "", err
	}
	return Updated, nil
}

func definitionHash(sys GameSystem) (string, error) {
	b, err := json.Marshal(struct {
		Name    string         `json:"name"`
		Family  string         `json:"family"`
		Aliases []string       `json:"aliases"`
		Gated   bool           `json:"gated"`
		Fields  []TrackerField `json:"fields"`
	}{sys.Name, sys.Family, sys.Aliases, sys.Gated, sys.Fields})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateSystem checks a definition is self-consistent: slug IDs and aliases, known field
// kinds, unique keys, bounds that make sense, and max references that resolve.
func ValidateSystem(sys GameSystem) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: system %q: %s", ErrInvalid, sys.ID, fmt.Sprintf(format, a...))
	}
	if !slug.MatchString(sys.ID) {
		return bad("id must be a lowercase slug")
	}
	if sys.Name == "" || sys.Family == "" || len(sys.Fields) == 0 {
		return bad("name, family and at least one field are required")
	}
	for _, a := range sys.Aliases {
		if !slug.MatchString(a) {
			return bad("alias %q must be a lowercase slug", a)
		}
	}
	byKey := map[string]TrackerField{}
	for _, f := range sys.Fields {
		if f.Key == "" || byKey[f.Key].Key != "" {
			return bad("field keys must be non-empty and unique (%q)", f.Key)
		}
		byKey[f.Key] = f
	}
	for _, f := range sys.Fields {
		if err := validateField(f, byKey); err != nil {
			return bad("%v", err)
		}
	}
	return nil
}

func validateField(f TrackerField, byKey map[string]TrackerField) error {
	switch f.Kind {
	case FieldCounter, FieldPool, FieldFlag, FieldTrack:
	default:
		return fmt.Errorf("field %q has unknown kind %q", f.Key, f.Kind)
	}
	if f.Max != nil && f.MaxRef != "" {
		return fmt.Errorf("field %q sets both max and max_ref", f.Key)
	}
	if f.Kind == FieldFlag && (f.Def < 0 || f.Def > 1 || f.MaxRef != "" ||
		(f.Min != nil && *f.Min < 0) || (f.Max != nil && *f.Max > 1)) {
		return fmt.Errorf("flag field %q must stay within 0 and 1", f.Key)
	}
	if f.MaxRef != "" {
		if ref, ok := byKey[f.MaxRef]; !ok || f.MaxRef == f.Key {
			return fmt.Errorf("field %q max_ref %q does not name another field", f.Key, f.MaxRef)
		} else if f.Def > ref.Def {
			return fmt.Errorf("field %q default exceeds its max_ref %q default", f.Key, f.MaxRef)
		}
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		return fmt.Errorf("field %q min exceeds max", f.Key)
	}
	if (f.Min != nil && f.Def < *f.Min) || (f.Max != nil && f.Def > *f.Max) {
		return fmt.Errorf("field %q default is out of bounds", f.Key)
	}
	return nil
}
