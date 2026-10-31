package connector

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// DecodeJSON strictly decodes a request body of at most limit bytes into v,
// answering the refusal itself when it fails.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		WriteError(w, &Error{Status: status, Message: "invalid request: " + err.Error()})
		return false
	}
	return true
}

// WriteError answers with err in Error's shape, which Client decodes. An
// *Error keeps its status (400 if unset); anything else is a 500.
func WriteError(w http.ResponseWriter, err error) {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		e = &Error{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	if e.Status == 0 {
		e.Status = http.StatusBadRequest
	}
	WriteJSON(w, e.Status, e)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}
