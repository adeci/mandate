// Package approval defines what the owner approves and how an approval is
// verified. A Record is stored as exact JSON bytes; those bytes go into the
// passkey challenge and are what the executor parses and runs, so what was
// approved and what runs can't drift apart.
package approval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const recordVersion = 1

// Record is one proposed action, as approved.
type Record struct {
	V         int               `json:"v"`
	ID        string            `json:"id"`
	Connector string            `json:"connector"`
	Op        string            `json:"op"`
	Args      map[string]string `json:"args"`
	// Display is rendered by the connector from Args, so it can't claim one
	// recipient while Args names another; executors recheck it.
	Display   []Field   `json:"display"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Field is one labelled line on the approval page. Param names the arg it
// shows when that arg may be edited before approving.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Param string `json:"param,omitempty"`
}

// Encode returns the canonical stored form of r.
func Encode(r Record) ([]byte, error) {
	r.V = recordVersion
	return json.Marshal(r)
}

// Decode parses stored record bytes, rejecting unknown fields and versions.
func Decode(b []byte) (Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("decode record: %w", err)
	}
	if dec.More() {
		return Record{}, errors.New("decode record: trailing data")
	}
	if r.V != recordVersion {
		return Record{}, fmt.Errorf("unsupported record version %d", r.V)
	}
	if r.ID == "" || r.Connector == "" || r.Op == "" {
		return Record{}, errors.New("record is missing id, connector, or op")
	}
	return r, nil
}
