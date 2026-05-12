package search

import (
	"strings"
	"unicode"
)

type queryOp int

const (
	queryOpAnd queryOp = iota
	queryOpOr
)

type parsedQuery struct {
	terms []string
	op    queryOp
}

// parseQuery splits raw into terms and detects AND vs OR intent.
//
// Rules:
//   - "|" between words means OR (any term must appear).
//   - Unquoted whitespace-separated words mean AND (all must appear).
//   - Double-quoted strings are treated as a single phrase term and matched
//     as a literal byte sequence, preserving existing search behavior.
func parseQuery(raw string) parsedQuery {
	if strings.Contains(raw, "|") {
		var terms []string
		for _, part := range strings.Split(raw, "|") {
			t := strings.ToLower(strings.TrimSpace(part))
			if t != "" {
				terms = append(terms, t)
			}
		}
		return parsedQuery{terms: terms, op: queryOpOr}
	}
	return parsedQuery{terms: tokenizeQuery(raw), op: queryOpAnd}
}

// tokenizeQuery splits a raw query string on whitespace, treating
// double-quoted spans as single phrase terms.
func tokenizeQuery(raw string) []string {
	var terms []string
	var buf strings.Builder
	inQuote := false

	for _, r := range raw {
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote && buf.Len() > 0 {
				terms = append(terms, strings.ToLower(buf.String()))
				buf.Reset()
			}
		case unicode.IsSpace(r) && !inQuote:
			if buf.Len() > 0 {
				terms = append(terms, strings.ToLower(buf.String()))
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		terms = append(terms, strings.ToLower(buf.String()))
	}
	return terms
}
