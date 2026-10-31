// Package connector is the protocol between mandate's gateway and console
// and the connectors that hold sessions, and the SDK to write one. A
// connector serves the protocol on a Unix socket:
//
//	GET  /spec     ops the connector offers
//	GET  /health   whether it is working or needs the owner
//	POST /setup    link it to the owner's account (console only)
//	POST /read     run a read op (no approval; results land in the connector's data.db)
//	POST /prepare  validate and normalize a write, render what the owner will see
//	POST /execute  run an approved write, verifying the approval itself
//	GET  /forward/{path...}  raw read, only for connectors that are a Forwarder
//
// A connector program is usually just a call to Main.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/adeci/mandate/approval"
)

// Kind separates ops agents may run directly from ops that need approval.
type Kind string

const (
	Read  Kind = "read"
	Write Kind = "write"
)

// ProtocolVersion is the protocol this package speaks. Server stamps it on
// every Spec; the gateway and console refuse a connector that speaks another.
const ProtocolVersion = 1

type Spec struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Ops      []Op   `json:"ops"`
	// Setup are the inputs the console offers the owner when linking the
	// connector, all optional, e.g. a phone number for a pairing code.
	Setup []Param `json:"setup,omitempty"`
	// Doc is markdown for agents beyond the ops, e.g. which API paths the
	// http connector allows.
	Doc string `json:"doc,omitempty"`
	// Forward is set by Server for a Forwarder: the gateway relays agents'
	// GET and HEAD to /forward/ without approval.
	Forward bool `json:"forward,omitempty"`
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
	// Container params name a container (chat, channel, ...); the gateway refuses the op
	// before it reaches the connector if that container is hidden.
	Container bool `json:"container,omitempty"`
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
	// Aliases are other IDs of the same container, e.g. a contact known by
	// two addresses; the write is refused if any of them is hidden.
	Aliases []string `json:"aliases,omitempty"`
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
	// Output is the service's response for the agent, e.g. an API body,
	// with credentials scrubbed.
	Output string `json:"output,omitempty"`
}

// ReadResult is what read ops return. Messages are stored in the connector's
// data.db first; the gateway answers with these IDs read back through its
// filtered views, so hidden and OTP rules apply to fetched data too.
type ReadResult struct {
	MessageIDs   []string `json:"message_ids,omitempty"`
	TotalResults int      `json:"total_results,omitempty"`
	// Containers counts containers upserted by sync-style reads.
	Containers int `json:"containers,omitempty"`
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
// It returns an encoded ReadResult.
type Reader interface {
	Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error)
}

// Forwarder is implemented by connectors that serve raw reads. Forward gets
// only GET and HEAD; path is the escaped request path after /forward/, not
// cleaned, so ".." and %2F reach the connector to refuse or keep.
type Forwarder interface {
	Forward(w http.ResponseWriter, r *http.Request, path string)
}

type HealthState string

const (
	Healthy HealthState = "ok"
	// NeedsSetup means the owner must act in the console, e.g. link an account.
	NeedsSetup HealthState = "setup"
	// Resting means reads are refused for pacing until Health.Until.
	Resting HealthState = "resting"
	Broken  HealthState = "down"
)

type Health struct {
	State  HealthState `json:"state"`
	Detail string      `json:"detail,omitempty"`
	Until  time.Time   `json:"until,omitzero"`
}

// Checker is implemented by connectors whose health can change; others are
// always Healthy.
type Checker interface {
	Health(ctx context.Context) Health
}

// SetupStep is one step of linking a connector, shown in the console.
type SetupStep struct {
	// QR is text to render as a QR code for the owner to scan.
	QR string `json:"qr,omitempty"`
	// Code is a code the owner types on another device.
	Code         string `json:"code,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Done         bool   `json:"done,omitempty"`
}

// Setupper is implemented by connectors the owner links from the console.
// Setup starts or continues linking; it must refuse while already linked.
type Setupper interface {
	Setup(ctx context.Context, args map[string]string) (SetupStep, error)
}

// Error is a request the connector refused, with a message meant for the
// agent (e.g. "ambiguous recipient"). Status is the HTTP status to relay.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	// RetryAfter is set when a read is refused for pacing.
	RetryAfter int `json:"retry_after_seconds,omitempty"`
	// Candidates are the matches for an ambiguous name. Message must not
	// name them: the gateway drops hidden ones before the agent sees them.
	Candidates []Candidate `json:"candidates,omitempty"`
}

type Candidate struct {
	// ID is the container the candidate resolves to; Aliases as in Prepared.
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	Label   string   `json:"label"`
}

func (e *Error) Error() string { return e.Message }

// Invalid is the error for bad args; its message goes back to the agent.
func Invalid(format string, a ...any) error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, a...)}
}

// ErrUnknownOp is returned for ops the connector doesn't offer.
var ErrUnknownOp = errors.New("unknown op")
