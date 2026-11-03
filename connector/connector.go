// Package connector is the connector protocol of docs/spec/connector.md:
// the SDK a Go connector is written with, usually just a call to Main, and
// the Client mandate calls connectors with.
package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/adeci/mandate/record"
)

// ProtocolVersion is the protocol this package speaks.
const ProtocolVersion = 1

// Connector is what a connector program implements. Reader, MediaFetcher
// and Unlinker add the optional routes.
type Connector interface {
	Spec() Spec
	Health(ctx context.Context) Health
	// Setup answers the owner's values for step with the next step; step ""
	// starts linking. It refuses with a 409 *Error while linked.
	Setup(ctx context.Context, step string, values map[string]string) (Step, error)
	// Prepare must be idempotent: preparing its own args returns the same
	// args and display, which is how Execute's caller detects tampering.
	Prepare(ctx context.Context, op string, args map[string]string) (Prepared, error)
	// Execute runs an approved write, at most once per approval.
	Execute(ctx context.Context, w record.Write) Result
}

// Reader is a connector with read ops.
type Reader interface {
	Read(ctx context.Context, op string, args map[string]string) (ReadResult, error)
}

// MediaFetcher is a connector that serves the files of stored messages. It
// stops downloading past limit bytes and refuses with TooLarge, since a
// sender can understate a file's size.
type MediaFetcher interface {
	Media(ctx context.Context, message string, index int, limit int64) (MediaFile, error)
}

// Unlinker is a connector that can forget its session and credentials,
// keeping stored messages; its health is then NeedsSetup.
type Unlinker interface {
	Unlink(ctx context.Context) error
}

type Spec struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Doc      string `json:"doc,omitempty"`
	// Media and Unlink are set from the optional interfaces.
	Media  bool `json:"media,omitempty"`
	Unlink bool `json:"unlink,omitempty"`
	Ops    []Op `json:"ops"`
}

// Kind separates ops that run inside an agent's grant from proposals.
type Kind string

const (
	Read  Kind = "read"
	Write Kind = "write"
)

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
	// Container params take a chat ID.
	Container bool `json:"container,omitempty"`
	// Message params take a message seq in the op's one container.
	Message bool `json:"message,omitempty"`
	// Editable params may be changed by the owner before approving.
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

type HealthState string

const (
	Healthy    HealthState = "ok"
	Connecting HealthState = "connecting"
	Resting    HealthState = "resting"
	NeedsSetup HealthState = "setup"
	Broken     HealthState = "down"
)

type Health struct {
	State HealthState `json:"state"`
	// Detail is shown to the owner and to agents: no credentials, no
	// message content.
	Detail string    `json:"detail,omitempty"`
	Until  time.Time `json:"until,omitzero"`
}

// ReadResult is what a read stored. The gateway reads the messages back
// through its filter.
type ReadResult struct {
	MessageIDs   []string `json:"message_ids,omitempty"`
	ContainerIDs []string `json:"container_ids,omitempty"`
	More         bool     `json:"more,omitempty"`
}

// Prepared is a write as execute will run it, what the owner sees and
// signs, and the chat and stored message it touches.
type Prepared struct {
	Args      map[string]string `json:"args"`
	Display   []record.Field    `json:"display"`
	Container string            `json:"container,omitempty"`
	Message   string            `json:"message,omitempty"`
}

type Status string

const (
	Done   Status = "done"
	Failed Status = "failed"
	// Unknown means the write may have reached the service; it is never
	// retried.
	Unknown Status = "unknown"
)

type Result struct {
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// Output is the service's answer for the agent, without credentials.
	Output string `json:"output,omitempty"`
}

// MediaFile is one file. Body yields exactly Size bytes; the server closes it.
type MediaFile struct {
	Type string
	Name string
	Size int64
	Body io.ReadCloser
}

// ReadMedia reads all of r, refusing it once it passes limit bytes.
func ReadMedia(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, TooLarge(limit)
	}
	return b, nil
}

// TooLarge refuses a file over limit bytes.
func TooLarge(limit int64) *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("the file is larger than the %d MiB limit", limit>>20)}
}

// Step is one step of the linking dialogue, rendered by the owner's app.
type Step struct {
	ID     string  `json:"step,omitempty"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Fields []Field `json:"fields,omitempty"`
	QR     string  `json:"qr,omitempty"`
	Code   string  `json:"code,omitempty"`
	// Confirm is a yes/no question, answered as values {"confirm": "yes"}.
	Confirm string `json:"confirm,omitempty"`
	// Poll is the seconds until the app sends the step again with no values.
	Poll int  `json:"poll,omitempty"`
	Done bool `json:"done,omitempty"`
}

type FieldKind string

const (
	TextField FieldKind = "text"
	// SecretField is typed hidden and never shown again.
	SecretField FieldKind = "secret"
	NumberField FieldKind = "number"
	ChoiceField FieldKind = "choice"
)

type Field struct {
	Name     string    `json:"name"`
	Label    string    `json:"label"`
	Kind     FieldKind `json:"kind"`
	Required bool      `json:"required,omitempty"`
	Options  []Option  `json:"options,omitempty"`
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Error is a refusal: Message is written for whoever sent the request and
// names only what they sent.
type Error struct {
	Status     int    `json:"-"`
	Message    string `json:"error"`
	RetryAfter int    `json:"retry_after_seconds,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Invalid refuses bad args with a 400.
func Invalid(format string, a ...any) error {
	return &Error{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, a...)}
}

// ErrUnknownOp is for an op the connector's spec does not list.
var ErrUnknownOp = &Error{Status: http.StatusNotFound, Message: "unknown op"}
