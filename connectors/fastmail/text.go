package main

import (
	"cmp"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const truncatedMarker = "[truncated; fetch reads the whole email]"

// render is the stored body of e: subject and recipients, the text up to
// limit bytes, then a line per attachment.
func render(e email, limit int) string {
	var head []string
	if e.Subject != "" {
		head = append(head, "Subject: "+oneLine(e.Subject))
	}
	if len(e.To) > 0 {
		head = append(head, "To: "+named(e.To))
	}
	if len(e.Cc) > 0 {
		head = append(head, "Cc: "+named(e.Cc))
	}
	text, cut := plainText(e, limit)
	var tail []string
	for _, p := range e.Attachments {
		tail = append(tail, "[attachment: "+oneLine(cmp.Or(p.Name, p.Type))+"]")
	}
	if cut {
		tail = append(tail, truncatedMarker)
	}
	var parts []string
	for _, p := range []string{strings.Join(head, "\n"), text, strings.Join(tail, "\n")} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

func named(as []address) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.Email
		if a.Name != "" {
			s[i] = oneLine(a.Name) + " <" + a.Email + ">"
		}
	}
	return strings.Join(s, ", ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// plainText joins e's text body parts, HTML converted, and reports whether
// any was cut short.
func plainText(e email, limit int) (string, bool) {
	var texts []string
	cut := false
	for _, p := range e.TextBody {
		v, ok := e.BodyValues[p.PartID]
		if !ok {
			continue
		}
		cut = cut || v.IsTruncated
		t := v.Value
		if p.Type == "text/html" {
			t = htmlText(t)
		}
		if t = tidy(t); t != "" {
			texts = append(texts, t)
		}
	}
	s := strings.Join(texts, "\n\n")
	if len(s) > limit {
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		s, cut = strings.TrimSpace(s[:limit]), true
	}
	return s, cut
}

var blankLines = regexp.MustCompile(`\n{3,}`)

// tidy trims trailing space from lines and keeps at most one blank line.
func tidy(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\u00a0")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// htmlText is the text an HTML body shows, with a link's target after its
// text and a line break for each block.
func htmlText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	hidden := 0 // depth inside head, script and style
	var href string
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			lines := strings.Split(b.String(), "\n")
			for i, l := range lines {
				lines[i] = strings.TrimSpace(l)
			}
			return strings.Join(lines, "\n")
		case html.TextToken:
			if hidden == 0 {
				b.WriteString(collapse(string(z.Text())))
			}
			continue
		case html.CommentToken, html.DoctypeToken:
			continue
		}
		name, hasAttr := z.TagName()
		tag := atom.Lookup(name)
		switch tag {
		case atom.Head, atom.Script, atom.Style, atom.Title:
			switch tt {
			case html.StartTagToken:
				hidden++
			case html.EndTagToken:
				hidden = max(hidden-1, 0)
			}
		case atom.Br:
			b.WriteString("\n")
		case atom.Li:
			if tt == html.StartTagToken {
				b.WriteString("\n- ")
			}
		case atom.A:
			switch tt {
			case html.StartTagToken:
				href = ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = string(v)
					}
				}
			case html.EndTagToken:
				if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
					b.WriteString(" (" + href + ")")
				}
				href = ""
			}
		case atom.P, atom.Div, atom.Tr, atom.Table, atom.Ul, atom.Ol, atom.Blockquote, atom.Pre, atom.Hr,
			atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			b.WriteString("\n")
		case atom.Td, atom.Th:
			if tt == html.StartTagToken {
				b.WriteString(" ")
			}
		}
	}
}

// collapse turns HTML whitespace runs into single spaces.
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

// threadName is subject without reply and forward prefixes.
func threadName(subject string) string {
	s := oneLine(subject)
	for {
		lower := strings.ToLower(s)
		cut := false
		for _, p := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(lower, p) {
				s, cut = strings.TrimSpace(s[len(p):]), true
				break
			}
		}
		if !cut {
			break
		}
	}
	return cmp.Or(s, "(no subject)")
}
