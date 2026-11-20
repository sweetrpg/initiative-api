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

func TestTheURIKeepsItsCredentialsByteForByteAndGainsTheTimeouts(t *testing.T) {
	raw := "mongodb+srv://u:p%40ss@cluster.example.test/appdb?retryWrites=true"
	t.Setenv("DB_URI", raw)
	ApplyDriverTimeouts()
	got := os.Getenv("DB_URI")
	if !strings.HasPrefix(got, raw+"&serverSelectionTimeoutMS=5000") || !strings.Contains(got, "connectTimeoutMS=5000") {
		t.Errorf("DB_URI = %q", got)
	}
	if bare := WithDriverTimeoutsURI("mongodb://h/db"); !strings.HasPrefix(bare, "mongodb://h/db?serverSelectionTimeoutMS=5000") {
		t.Errorf("URI without a query gave %q", bare)
	}
}

func TestTheOptionsAreUsedWhenThereIsNoURI(t *testing.T) {
	t.Setenv("DB_OPTS", "w=majority")
	if v, ok := os.LookupEnv("DB_URI"); ok {
		t.Setenv("DB_URI", v) // restored after the test
		if err := os.Unsetenv("DB_URI"); err != nil {
			t.Fatal(err)
		}
	}
	ApplyDriverTimeouts()
	if got := os.Getenv("DB_OPTS"); !strings.Contains(got, "w=majority") || !strings.Contains(got, "connectTimeoutMS=5000") {
		t.Errorf("DB_OPTS = %q", got)
	}
}
