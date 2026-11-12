package data

import "errors"

// Errors the API layer maps to HTTP statuses.
var (
	// ErrNotFound: no live record with that ID is visible to the caller (404).
	ErrNotFound = errors.New("data: not found")
	// ErrForbidden: the record exists but belongs to another owner (403). Reported only after the
	// owner-scoped write has already failed to match.
	ErrForbidden = errors.New("data: record belongs to another owner")
	// ErrRevisionConflict: the write carried a stale revision (409). The current record is
	// returned alongside it.
	ErrRevisionConflict = errors.New("data: revision conflict")
	// ErrConflict: a unique index rejected the write, for example a theme slot already in use (409).
	ErrConflict = errors.New("data: conflicts with an existing record")
	// ErrInactive: the record is over the owner's current limit and cannot be changed or used; it
	// stays readable and deletable (403).
	ErrInactive = errors.New("data: record is inactive")
	// ErrInvalid: the call itself is malformed (empty owner, reserved field, bad query) (400/422).
	ErrInvalid = errors.New("data: invalid argument")
)
