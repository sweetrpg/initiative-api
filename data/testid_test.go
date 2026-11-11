package data

import "fmt"

// testID maps a short label ("a", "b", ...) to a valid 24-hex ObjectID string, since the data
// layer rejects any record ID that is not one.
func testID(label string) string { return fmt.Sprintf("%024x", label[0]) }
