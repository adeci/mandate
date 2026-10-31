// Package exitcode maps errors to process exit codes scripts and agents can act on.
package exitcode

import "errors"

const (
	Usage = 2
	// TryLater is EX_TEMPFAIL from sysexits.h.
	TryLater = 75
)

var (
	ErrTryLater = errors.New("try again later")
	ErrUsage    = errors.New("usage")
)

func For(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrTryLater):
		return TryLater
	case errors.Is(err, ErrUsage):
		return Usage
	default:
		return 1
	}
}
