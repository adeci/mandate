// Package exitcode maps errors to process exit codes systemd and scripts can act on.
package exitcode

import "errors"

const (
	// NeedsOwner is in the unit's RestartPreventExitStatus, so the unit stays
	// failed instead of restarting forever.
	NeedsOwner = 3
	Usage      = 2
	// TryLater is EX_TEMPFAIL from sysexits.h.
	TryLater = 75
)

var (
	ErrNeedsOwner = errors.New("owner action required")
	ErrTryLater   = errors.New("try again later")
	ErrUsage      = errors.New("usage")
)

func For(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrNeedsOwner):
		return NeedsOwner
	case errors.Is(err, ErrTryLater):
		return TryLater
	case errors.Is(err, ErrUsage):
		return Usage
	default:
		return 1
	}
}
