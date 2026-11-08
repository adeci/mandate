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
		Raw    string   `json:"raw"`
		Name   string   `json:"name"`
		Words  []string `json:"words"`
		Digits string   `json:"digits"`
	}
	ts := make([]term, len(terms))
	for i, t := range terms {
		p := term{Raw: strings.ToLower(strings.TrimSpace(t)), Words: Words(t)}
		p.Name = strings.Join(p.Words, " ")
		p.Digits = number(p.Raw)
		ts[i] = p
	}
	b, err := json.Marshal(ts)
	return string(b), err
}

// Words splits a term as Matching does: lowercase, on spaces and "/", with
// a leading "#" or "~" dropped from each word.
func Words(term string) []string {
	words := []string{}
	for _, w := range strings.FieldsFunc(strings.ToLower(term), func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
		if w = strings.TrimLeft(w, "#~"); w != "" {
			words = append(words, w)
		}
	}
	return words
}

// number is the digits of a term written as a phone number: at least 5
// digits, with nothing else but "+", spaces, dashes, dots and parentheses.
// Any other term has none.
func number(term string) string {
	var digits []byte
	for _, r := range term {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, byte(r))
		case !unicode.IsSpace(r) && !strings.ContainsRune("+-.()", r):
			return ""
		}
	}
	if len(digits) < 5 {
		return ""
	}
	return string(digits)
}

// HolderKinds, as a SQL list, are the kinds of chat that hold other chats
// and no messages of their own: Discord's servers and its forum and media
// channels, whose posts are threads, and Matrix spaces. A grant on one
// covers every chat under it, and agents never see them listed.
const HolderKinds = `('guild', 'space', 'forum', 'media')`

// Holder says whether chats of kind are holders.
func Holder(kind string) bool {
	return kind != "" && strings.Contains(HolderKinds, "'"+kind+"'")
}

// Matching is how a search term finds chats, for an agent's request and
// the owner's chat picker alike. It continues a WITH clause that defines
// candidates(id, kind, parent_id, own, path, handle, at): each chat with
// its own name, its path (its parents' names and its own, "main /
// #general"), a DM's handle ("@sam_p", "+15551234567") and its last
// activity; a chat with neither path nor handle matches by ID only.
// termsJSON binds the JSON of Terms, and now the time it is, as SQLite's
// date functions read it. It defines matched(k, id, n): the chats term k
// found, ranked n from 1.
//
// A term is words, split on spaces and "/", a leading "#" or "~" ignored. A
// chat matches when each word is in its path, so "main / general", "main
// general" and "main #general" find main's #general. A DM's handle counts
// as part of its path, so "sam_p", "@sam_p" and "@bob:example.org" find the
// DM with that person, except for words that are all digits: like digits in
// an ID, they would find most numbers. A chat also matches when the term is
// its ID, or the part of it before "@". A term written as a phone number,
// "+1 (555) 999-0002", compares digits only: it finds the chats whose path
// or handle holds them, and those whose name, handle or ID is that number,
// with or without a country code of up to 3 digits. A chat under another
// match, by the parents stored in containers, is left out: the thread of a
// matching channel matches the same words.
//
// Chats idle for over a year, or never active, rank after all others.
// Within both, the chat whose ID is the term comes first, then chats
// matching by their own name, then the most recent, a mailbox or holder
// being as recent as the newest chat in it.
//
// A term naming a holder or a mailbox exactly finds it alone, as a grant on
// it covers the chats filed under it later. Holders match no other way:
// they hold no messages.
func Matching(termsJSON, now string) string {
	// hit is materialized: inlined, SQLite would run it again for each chat
	// it checks for a match above it. Each check is an equality, which
	// SQLite indexes; with an IN list it scans every hit.
	return fmt.Sprintf(`term(k, raw, name, words, digits) AS (
  SELECT key, value ->> 'raw', value ->> 'name', value -> 'words', value ->> 'digits' FROM json_each(%[1]s)),
pathed AS (
  SELECT c.id, c.kind, c.parent_id AS p1, a1.parent_id AS p2, a2.parent_id AS p3, c.at,
         lower(COALESCE(c.own, '')) AS own_l, lower(COALESCE(c.path, '')) AS path_l, lower(COALESCE(c.handle, '')) AS handle_l, lower(c.id) AS id_l,
         lower(substr(c.id, 1, iif(instr(c.id, '@') > 1, instr(c.id, '@') - 1, length(c.id)))) AS user_l,
         %[4]s AS own_d, %[5]s AS path_d, %[8]s AS handle_d
  FROM candidates AS c
  LEFT JOIN containers AS a1 ON a1.id = c.parent_id
  LEFT JOIN containers AS a2 ON a2.id = a1.parent_id),
newest(id, at) AS MATERIALIZED (
  SELECT id, max(at) FROM (SELECT p1 AS id, at FROM pathed UNION ALL SELECT p2, at FROM pathed UNION ALL SELECT p3, at FROM pathed)
  WHERE id IS NOT NULL GROUP BY id),
hit AS MATERIALIZED (
  SELECT term.k, l.id, l.p1, l.p2, l.p3,
         iif(l.kind = 'mailbox' OR l.kind IN %[2]s,
           NULLIF(max(COALESCE(l.at, ''), COALESCE((SELECT n.at FROM newest AS n WHERE n.id = l.id), '')), ''), l.at) AS at,
         term.raw IN (l.id_l, l.user_l) OR term.digits != '' AND term.digits = ltrim(l.user_l, '+') AS same_id,
         (l.kind = 'mailbox' OR l.kind IN %[2]s) AND ltrim(l.own_l, '#~') = term.name AS alone,
         iif(term.digits = '', NOT EXISTS (SELECT 1 FROM json_each(term.words) AS w WHERE instr(l.own_l, w.value) = 0),
           instr(l.own_d, term.digits) > 0 OR %[6]s) AS own
  FROM term JOIN pathed AS l
    ON l.kind IN %[2]s AND term.name != '' AND ltrim(l.own_l, '#~') = term.name
    OR l.kind NOT IN %[2]s AND (
      term.raw IN (l.id_l, l.user_l)
      OR term.digits = '' AND term.name != '' AND (l.path_l != '' OR l.handle_l != '') AND NOT EXISTS (SELECT 1 FROM json_each(term.words) AS w
        WHERE instr(l.path_l, w.value) = 0 AND (instr(l.handle_l, w.value) = 0 OR w.value NOT GLOB '*[^0-9]*'))
      OR term.digits != '' AND (instr(l.path_d, term.digits) > 0 OR instr(l.handle_d, term.digits) > 0 OR %[6]s OR %[7]s OR %[9]s))),
single(k) AS (SELECT DISTINCT k FROM hit WHERE alone),
picked AS (
  SELECT k, id, same_id, own, at FROM hit AS h
  WHERE (alone OR k NOT IN (SELECT k FROM single))
    AND NOT EXISTS (SELECT 1 FROM hit AS a WHERE a.k = h.k AND a.id = h.p1)
    AND NOT EXISTS (SELECT 1 FROM hit AS a WHERE a.k = h.k AND a.id = h.p2)
    AND NOT EXISTS (SELECT 1 FROM hit AS a WHERE a.k = h.k AND a.id = h.p3)),
matched(k, id, n) AS (
  SELECT k, id, row_number() OVER (PARTITION BY k ORDER BY COALESCE(julianday(at) < julianday(%[3]s, '-1 year'), 1),
    same_id DESC, own DESC, at DESC NULLS LAST, id) FROM picked)`,
		termsJSON, HolderKinds, now, strip("COALESCE(c.own, '')"), strip("COALESCE(c.path, '')"),
		sameNumber("l.own_d"), sameNumber("ltrim(l.user_l, '+')"), strip("COALESCE(c.handle, '')"), sameNumber("l.handle_d"))
}

// strip is the SQL text x without what people write between a phone
// number's digits.
func strip(x string) string {
	for _, c := range []string{" ", "-", ".", "(", ")", "+"} {
		x = "replace(" + x + ", '" + c + "', '')"
	}
	return x
}

// sameNumber is SQL for whether x, a chat's name or ID without a number's
// punctuation, is the number in the term's digits, give or take a country
// code of up to 3 digits that one of them lacks.
func sameNumber(x string) string {
	return fmt.Sprintf(`(length(%[1]s) >= 5 AND %[1]s NOT GLOB '*[^0-9]*' AND abs(length(%[1]s) - length(term.digits)) <= 3
        AND substr(%[1]s, -min(length(%[1]s), length(term.digits))) = substr(term.digits, -min(length(%[1]s), length(term.digits))))`, x)
}
