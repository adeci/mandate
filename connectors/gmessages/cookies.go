package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The cookies mautrix-gmessages asks for; the rest of a browser's cookie jar
// stays out of the session.
var (
	requiredCookies = []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "OSID"}
	optionalCookies = []string{"__Secure-1PSIDTS"}
)

// parseCookies reads a Cookie request header (with or without the
// "Cookie:" name), a "Copy as cURL" command, or a JSON object of names to
// values. Pasted into a one-line input, a cURL command has lost its line
// breaks, which the parser tolerates.
func parseCookies(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	var all map[string]string
	switch {
	case strings.HasPrefix(s, "{"):
		if err := json.Unmarshal([]byte(s), &all); err != nil {
			return nil, errors.New("the cookies look like JSON but are not an object of names to text values")
		}
	case strings.HasPrefix(s, "curl "):
		words, err := shellWords(s)
		if err != nil {
			return nil, err
		}
		all = make(map[string]string)
		for i := 1; i < len(words); i++ {
			switch w := words[i]; {
			case (w == "-b" || w == "--cookie") && i+1 < len(words):
				i++
				cookieHeader(words[i], all)
			case (w == "-H" || w == "--header") && i+1 < len(words):
				i++
				if name, value, ok := strings.Cut(words[i], ":"); ok && strings.EqualFold(strings.TrimSpace(name), "cookie") {
					cookieHeader(value, all)
				}
			}
		}
	default:
		if name, value, ok := strings.Cut(s, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "cookie") {
			s = value
		}
		all = make(map[string]string)
		cookieHeader(s, all)
	}

	out := make(map[string]string)
	var missing []string
	for _, name := range requiredCookies {
		if v := all[name]; v != "" {
			out[name] = v
		} else {
			missing = append(missing, name)
		}
	}
	for _, name := range optionalCookies {
		if v := all[name]; v != "" {
			out[name] = v
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing cookies %s: copy the Cookie header of a request to messages.google.com while signed in", strings.Join(missing, ", "))
	}
	return out, nil
}

// cookieHeader adds the name=value pairs of a Cookie header to into.
func cookieHeader(h string, into map[string]string) {
	for pair := range strings.SplitSeq(h, ";") {
		name, value, ok := strings.Cut(pair, "=")
		if name = strings.TrimSpace(name); ok && name != "" {
			into[name] = strings.TrimSpace(value)
		}
	}
}

// shellWords splits a command as a POSIX shell would for the quoting
// browsers produce: '...', "...", $'...' and backslash escapes. A backslash
// before whitespace is a line continuation whose newline was lost.
func shellWords(s string) ([]string, error) {
	var words []string
	var w strings.Builder
	inWord := false
	end := func() {
		if inWord {
			words = append(words, w.String())
			w.Reset()
			inWord = false
		}
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		switch c := r[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			end()
		case c == '\\':
			if i+1 < len(r) && !slices.Contains([]rune{' ', '\t', '\n', '\r'}, r[i+1]) {
				i++
				w.WriteRune(r[i])
				inWord = true
			}
		case c == '\'':
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j == len(r) {
				return nil, errors.New("the cURL command has an unclosed quote")
			}
			w.WriteString(string(r[i+1 : j]))
			inWord, i = true, j
		case c == '"' || c == '$' && i+1 < len(r) && r[i+1] == '\'':
			quote := c
			if c == '$' {
				quote, i = '\'', i+1
			}
			j := i + 1
			for ; j < len(r) && r[j] != quote; j++ {
				if r[j] == '\\' && j+1 < len(r) {
					j++
				}
				w.WriteRune(r[j])
			}
			if j == len(r) {
				return nil, errors.New("the cURL command has an unclosed quote")
			}
			inWord, i = true, j
		default:
			w.WriteRune(c)
			inWord = true
		}
	}
	end()
	return words, nil
}
