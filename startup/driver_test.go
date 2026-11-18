package startup

import (
	"os"
	"strings"
	"testing"
)

func TestDriverTimeoutsAreAddedOnlyWhereTheSettingsLeaveThemOut(t *testing.T) {
	got := WithDriverTimeouts("retryWrites=true&SOCKETTIMEOUTMS=1000")
	for _, want := range []string{"retryWrites=true", "SOCKETTIMEOUTMS=1000", "serverSelectionTimeoutMS=5000", "connectTimeoutMS=5000"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "socketTimeoutMS=30000") {
		t.Errorf("%q overrode a timeout the settings already set", got)
	}
	if empty := WithDriverTimeouts(""); !strings.HasPrefix(empty, "serverSelectionTimeoutMS=5000&") {
		t.Errorf("empty query gave %q", empty)
	}
}

func TestTheTimeoutsReachTheURIOrTheOptionsTheDatabasePackageReads(t *testing.T) {
	t.Setenv("DB_URI", "mongodb+srv://u:p@cluster.example.test/appdb?retryWrites=true")
	ApplyDriverTimeouts()
	if got := envOf("DB_URI"); !strings.Contains(got, "serverSelectionTimeoutMS=5000") || !strings.Contains(got, "retryWrites=true") {
		t.Errorf("DB_URI = %q", got)
	}
	t.Setenv("DB_URI", "")
	if err := unsetEnv("DB_URI"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_OPTS", "w=majority")
	ApplyDriverTimeouts()
	if got := envOf("DB_OPTS"); !strings.Contains(got, "w=majority") || !strings.Contains(got, "connectTimeoutMS=5000") {
		t.Errorf("DB_OPTS = %q", got)
	}
}

func envOf(k string) string { return os.Getenv(k) }

func unsetEnv(k string) error { return os.Unsetenv(k) }
