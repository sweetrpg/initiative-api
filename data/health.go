package data

import "fmt"

// HealthError reports the health field that failed validation. It wraps ErrInvalid.
type HealthError struct {
	Field  string
	Reason string
}

func (e *HealthError) Error() string { return fmt.Sprintf("health field %q: %s", e.Field, e.Reason) }

// Unwrap lets errors.Is match ErrInvalid.
func (e *HealthError) Unwrap() error { return ErrInvalid }

// CheckHealth validates h against sys's tracker definition: every key must be a defined field, and
// each value must be at least the field's Min and at most its Max (or, for MaxRef, the current
// value of the referenced field in h when present).
func CheckHealth(sys GameSystem, h map[string]int) error {
	fields := make(map[string]TrackerField, len(sys.Fields))
	for _, f := range sys.Fields {
		fields[f.Key] = f
	}
	for k, v := range h {
		f, ok := fields[k]
		if !ok {
			return &HealthError{k, "not defined by the game system"}
		}
		if f.Min != nil && v < *f.Min {
			return &HealthError{k, fmt.Sprintf("%d is below the minimum %d", v, *f.Min)}
		}
		if f.Max != nil && v > *f.Max {
			return &HealthError{k, fmt.Sprintf("%d is above the maximum %d", v, *f.Max)}
		}
		if ref, ok := h[f.MaxRef]; f.MaxRef != "" && ok && v > ref {
			return &HealthError{k, fmt.Sprintf("%d is above %s (%d)", v, f.MaxRef, ref)}
		}
	}
	return nil
}
