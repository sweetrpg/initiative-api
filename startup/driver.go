package startup

import (
	"net/url"
	"os"
	"strings"
)

// Driver timeouts applied when the connection settings do not set them. Without them a dead
// database holds every request for the driver's 30s server-selection wait, and an operation on a
// half-open connection waits forever.
var driverDefaults = []struct{ key, value string }{
	{"serverSelectionTimeoutMS", "5000"},
	{"connectTimeoutMS", "5000"},
	{"socketTimeoutMS", "30000"},
}

// WithDriverTimeouts adds the default driver timeouts to a connection string's query for each one
// it does not already name (names compare case-insensitively, as the driver does).
func WithDriverTimeouts(rawQuery string) string {
	have := map[string]bool{}
	for _, kv := range strings.Split(rawQuery, "&") {
		if k, _, _ := strings.Cut(kv, "="); k != "" {
			have[strings.ToLower(k)] = true
		}
	}
	out := rawQuery
	for _, d := range driverDefaults {
		if have[strings.ToLower(d.key)] {
			continue
		}
		if out != "" {
			out += "&"
		}
		out += d.key + "=" + d.value
	}
	return out
}

// ApplyDriverTimeouts puts the default timeouts into the connection settings the database package
// reads from the environment: the query of DB_URI when that is set, DB_OPTS otherwise.
func ApplyDriverTimeouts() {
	if raw, ok := os.LookupEnv("DB_URI"); ok {
		if u, err := url.Parse(raw); err == nil {
			u.RawQuery = WithDriverTimeouts(u.RawQuery)
			_ = os.Setenv("DB_URI", u.String())
		}
		return
	}
	_ = os.Setenv("DB_OPTS", WithDriverTimeouts(os.Getenv("DB_OPTS")))
}
