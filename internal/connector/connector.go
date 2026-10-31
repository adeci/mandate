// Package connector is the protocol between mandate's gateway and approvals
// units and the per-service connectors that hold sessions. A connector serves
// it on a Unix socket:
//
//	GET  /spec     ops the connector offers
//	POST /read     run a read op (no approval; results land in the connector's data.db)
//	POST /prepare  validate and normalize a write, render what the owner will see
//	POST /execute  run an approved write, verifying the approval itself
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/adeci/mandate/internal/approval"
)

// Kind separates ops agents may run directly from ops that need approval.
type Kind string

const (
	Read  Kind = "read"
	Write Kind = "write"
)

type Spec struct {
	Name string `json:"name"`
	Ops  []Op   `json:"ops"`
}

type Op struct {
	Name    string  `json:"name"`
	Kind    Kind    `json:"kind"`
	Summary string  `json:"summary"`
	Params  []Param `json:"params"`
}

type Param struct {
	Name     string `json:"name"`
	Doc      string `json:"doc"`
	Required bool   `json:"required,omitempty"`
	// Editable params can be changed by the owner on the approval page.
	Editable bool `json:"editable,omitempty"`
}

func (s Spec) Op(name string) (Op, bool) {
	for _, op := range s.Ops {
		if op.Name == name {
			return op, true
		}
	}
	return Op{}, false
}

// Prepared is a write after normalization: Args with names resolved to IDs,
// the fields the owner must see, and the container it writes to (for exposure
// checks).
type Prepared struct {
	Args      map[string]string `json:"args"`
	Display   []approval.Field  `json:"display"`
	Container string            `json:"container,omitempty"`
}

type Status string

const (
	Done   Status = "done"
	Failed Status = "failed"
	// Unknown means the request may have reached the upstream service. It is
	// never retried, so a message is sent at most once.
	Unknown Status = "unknown"
)

type Result struct {
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Ref identifies what was created upstream, e.g. a message ID.
	Ref string `json:"ref,omitempty"`
}

// Connector is implemented once per service.
type Connector interface {
	Spec() Spec
	// Prepare must be idempotent: preparing its own output returns the same
	// args and display, which is how executors detect tampering.
	Prepare(ctx context.Context, op string, args map[string]string) (Prepared, error)
	// Execute runs an approved write. It is called at most once per approval.
	Execute(ctx context.Context, rec approval.Record) Result
}

// Reader is implemented by connectors with read ops, e.g. on-demand fetches.
type Reader interface {
	Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error)
}

// Error is a request the connector refused, with a message meant for the
// agent (e.g. "ambiguous recipient"). Status is the HTTP status to relay.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	// RetryAfter is set when a read is refused for pacing.
	RetryAfter int `json:"retry_after_seconds,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Invalid is the error for bad args; its message goes back to the agent.
func Invalid(format string, a ...any) error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, a...)}
}

// ErrUnknownOp is returned for ops the connector doesn't offer.
var ErrUnknownOp = errors.New("unknown op")
