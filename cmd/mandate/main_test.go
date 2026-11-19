package main

import (
	"errors"
	"strings"
	"testing"
)

// A server command with the wrong arguments answers the usage, never the
// agent CLI.
func TestServerCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"setup", "--help"}, {"reset", "--bogus"}, {"reset", "--unlink", "x"}, {"store-acl"}} {
		if err := run(t.Context(), args); !errors.Is(err, errUsage) {
			t.Errorf("mandate %s: %v, want the usage", strings.Join(args, " "), err)
		}
	}
}
