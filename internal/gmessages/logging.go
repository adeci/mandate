package gmessages

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/rs/zerolog"
)

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func libgmLogger(log *slog.Logger) zerolog.Logger {
	return zerolog.New(slogWriter{log: log.With("component", "libgm")}).Level(zerolog.InfoLevel)
}

// These libgm log fields can carry decrypted message contents.
var redactedFields = []string{"data", "evt_data", "decrypted_data", "resp_body", "devices"}

// slogWriter receives one zerolog JSON event per Write.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	fields, ok := decodeEvent(p)
	if !ok {
		// A log sink must never fail the library's write.
		w.log.Warn("unparseable library log line")
		return len(p), nil
	}
	level := slog.LevelInfo
	switch fields[zerolog.LevelFieldName] {
	case "trace", "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error", "fatal", "panic":
		level = slog.LevelError
	}
	msg, _ := fields[zerolog.MessageFieldName].(string)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		switch k {
		case zerolog.LevelFieldName, zerolog.MessageFieldName, zerolog.TimestampFieldName:
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	attrs := make([]any, 0, len(keys))
	for _, k := range keys {
		if slices.Contains(redactedFields, strings.ToLower(k)) {
			continue
		}
		attrs = append(attrs, slog.Any(k, fields[k]))
	}
	w.log.Log(context.Background(), level, msg, attrs...)
	return len(p), nil
}

func decodeEvent(p []byte) (map[string]any, bool) {
	var fields map[string]any
	return fields, json.Unmarshal(p, &fields) == nil
}
