// Package exitcode maps errors to process exit codes systemd can act on.
package exitcode

import "errors"

// NeedsOwner is in the unit's RestartPreventExitStatus, so the unit stays
// failed instead of restarting forever.
const NeedsOwner = 3

var ErrNeedsOwner = errors.New("owner action required")

func For(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrNeedsOwner):
		return NeedsOwner
	default:
		return 1
	}
}
