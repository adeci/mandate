package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Terms encodes search terms for Matching.
func Terms(terms []string) (string, error) {
	type term struct {
		Raw   string   `json:"raw"`
		Name  string   `json:"name"`
		Words []string `json:"words"`
	}
	ts := make([]term, len(terms))
	for i, t := range terms {
		p := term{Raw: strings.ToLower(strings.TrimSpace(t)), Words: []string{}}
		for _, w := range strings.FieldsFunc(p.Raw, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
			if w = strings.TrimLeft(w, "#~"); w != "" {
				p.Words = append(p.Words, w)
			}
		}
		p.Name = strings.Join(p.Words, " ")
		ts[i] = p
	}
	b, err := json.Marshal(ts)
	return string(b), err
}

// Matching is how a search term finds chats, for an agent's request and
// the owner's chat picker alike. It continues a WITH clause that defines
// candidates(id, kind, parent_id, own, path, at): each chat with its own
// name, its path (its parents' names and its own, "main / #general"; empty
// to match by ID only) and its last activity. termsJSON binds the JSON of
// Terms. It defines matched(k, id, n): the chats term k found, ranked n
// from 1.
//
// A term is words, split on spaces and "/", a leading "#" or "~" ignored. A
// chat matches when each word is in its path, so "main / general", "main
// general" and "main #general" find main's #general; or when the term is in
// its ID. A chat under another match is left out: the thread of a matching
// channel matches the same words. The chat whose ID is the term comes first,
// then chats matching by their own name, then the most recent.
//
// A term naming a mailbox exactly finds it alone, as a grant on it covers
// the mail filed later. Guilds, spaces and categories hold no messages and
// a grant on one covers nothing under it, so they never match: a term
// naming one exactly finds the chats directly in it instead.
func Matching(termsJSON string) string {
	return fmt.Sprintf(`term(k, raw, name, words) AS (
  SELECT key, value ->> 'raw', value ->> 'name', value -> 'words' FROM json_each(%[1]s)),
pathed AS (
  SELECT *, lower(COALESCE(own, '')) AS own_l, lower(COALESCE(path, '')) AS path_l FROM candidates),
hit AS (
  SELECT term.k, l.id, l.at, l.path_l, lower(l.id) = term.raw AS same_id,
         l.kind = 'mailbox' AND ltrim(l.own_l, '#~') = term.name AS alone,
         NOT EXISTS (SELECT 1 FROM json_each(term.words) AS w WHERE instr(l.own_l, w.value) = 0) AS own
  FROM term JOIN pathed AS l
    ON l.kind NOT IN %[2]s AND (
      term.name != '' AND l.path_l != '' AND NOT EXISTS (SELECT 1 FROM json_each(term.words) AS w WHERE instr(l.path_l, w.value) = 0)
      OR term.raw != '' AND instr(lower(l.id), term.raw))),
holder(k, id) AS (
  SELECT term.k, l.id FROM term JOIN pathed AS l
  WHERE term.name != '' AND l.kind IN %[2]s AND ltrim(l.own_l, '#~') = term.name),
single(k) AS (SELECT DISTINCT k FROM hit WHERE alone),
picked AS (
  SELECT k, id, same_id, own, at FROM hit AS h
  WHERE k NOT IN (SELECT k FROM holder) AND (alone OR k NOT IN (SELECT k FROM single))
    AND NOT EXISTS (SELECT 1 FROM hit AS a WHERE a.k = h.k AND a.path_l != ''
      AND substr(h.path_l, 1, length(a.path_l) + 3) = a.path_l || ' / ')
  UNION ALL
  SELECT holder.k, l.id, 0, 1, l.at FROM holder JOIN pathed AS l ON l.parent_id = holder.id
  WHERE l.kind NOT IN %[2]s),
matched(k, id, n) AS (
  SELECT k, id, row_number() OVER (PARTITION BY k ORDER BY same_id DESC, own DESC, at DESC NULLS LAST, id) FROM picked)`,
		termsJSON, `('guild', 'space', 'category')`)
}
