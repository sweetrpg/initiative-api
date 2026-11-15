package data

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Size bounds on stored text and collections. Every write path checks them in the data layer, so no
// route can store an unbounded document; a request over a bound is a typed 422 (ErrInvalid).
const (
	MaxNameLen    = 200  // runes: roster, encounter, combatant and character names, theme_id
	MaxNotesLen   = 4000 // runes
	MaxCombatants = 200  // per encounter, removed ones included
	MaxCharacters = 200  // per roster
	MaxConditions = 20   // per combatant
	MaxMapEntries = 32   // health values and external refs per combatant or character
	MaxThemeIDLen = 64
)

// CheckText requires s to be at most max runes. what names the field in the error.
func CheckText(what, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%w: %s is longer than %d characters", ErrInvalid, what, max)
	}
	return nil
}

// CheckName requires a non-empty (after trimming) name of at most MaxNameLen runes.
func CheckName(what, s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, what)
	}
	return CheckText(what, s, MaxNameLen)
}

// CheckMaps bounds the health and external-ref maps of a combatant or character: at most
// MaxMapEntries entries each, and keys and values no longer than maxRefLen.
func CheckMaps(what string, health map[string]int, refs map[string]string) error {
	if len(health) > MaxMapEntries || len(refs) > MaxMapEntries {
		return fmt.Errorf("%w: %s has more than %d health values or external references", ErrInvalid, what, MaxMapEntries)
	}
	for k := range health {
		if err := CheckText(what+" health key", k, maxRefLen); err != nil {
			return err
		}
	}
	for k, v := range refs {
		if err := CheckText(what+" external reference key", k, maxRefLen); err != nil {
			return err
		}
		if err := CheckText(what+" external reference value", v, maxRefLen); err != nil {
			return err
		}
	}
	return nil
}
