package data

import (
	"context"

	"github.com/sweetrpg/initiative-api/relations"
)

// Decision is a granted authorization for one user on one session (PADR-0035). Every session data
// function takes one and has no unchecked form: the only way to get a granted Decision is
// Authorize, which asks auth-api (OpenFGA) with the caller's own bearer token. The zero value
// grants nothing.
type Decision struct {
	actor, session, relation string
}

// Actor returns the user the decision was made for.
func (d Decision) Actor() string { return d.actor }

// SessionID returns the session the decision covers.
func (d Decision) SessionID() string { return d.session }

// check refuses the zero Decision, so a forgotten check cannot read or write session data.
func (d Decision) check() error {
	if d.actor == "" || d.session == "" || d.relation == "" {
		return ErrForbidden
	}
	return nil
}

// RelationChecker answers whether the holder of token has relation on object.
// *relations.Client implements it.
type RelationChecker interface {
	Check(ctx context.Context, user, token, object, relation string) (bool, error)
}

type gate struct {
	checker RelationChecker
	token   string
}

type gateKey struct{}

// WithGate puts the checker and the caller's bearer token on ctx for Authorize. Per PADR-0037 the
// token is the caller's own; there is no service token.
func WithGate(ctx context.Context, c RelationChecker, token string) context.Context {
	return context.WithValue(ctx, gateKey{}, gate{c, token})
}

// Authorize asks whether actor holds relation on the session. A malformed session id is
// ErrNotFound; a denial is ErrForbidden and carries no session data. When no decision can be
// obtained (no gate on ctx, auth-api or OpenFGA down) the error is relations.ErrUnavailable, or
// relations.ErrUnauthorized for a rejected token: session data fails closed.
func Authorize(ctx context.Context, actor, sessionID, relation string) (Decision, error) {
	if err := checkUserID(actor); err != nil {
		return Decision{}, err
	}
	id, ok := cleanID(sessionID)
	if !ok {
		return Decision{}, ErrNotFound
	}
	g, ok := ctx.Value(gateKey{}).(gate)
	if !ok || g.checker == nil {
		return Decision{}, relations.ErrUnavailable
	}
	allowed, err := g.checker.Check(ctx, actor, g.token, "session:"+id, relation)
	if err != nil {
		return Decision{}, err
	}
	if !allowed {
		return Decision{}, ErrForbidden
	}
	return Decision{actor: actor, session: id, relation: relation}, nil
}
