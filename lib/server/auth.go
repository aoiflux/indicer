package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Scope is a set of things a token may do.
type Scope uint8

const (
	ScopeRead Scope = 1 << iota
	ScopeUpload
	ScopeExport

	// ScopeAll is what "all" grants in a v1 token file. It is fixed, so a
	// scope added later is not granted to tokens written before it.
	ScopeAll = ScopeRead | ScopeUpload | ScopeExport
)

const scopeAllName = "all"

var scopeNames = []struct {
	scope Scope
	name  string
}{{ScopeRead, "read"}, {ScopeUpload, "upload"}, {ScopeExport, "export"}}

// names lists the scopes in s, in a fixed order.
func (s Scope) names() []string {
	var out []string
	for _, n := range scopeNames {
		if s&n.scope != 0 {
			out = append(out, n.name)
		}
	}
	return out
}

// String names the scopes in s, comma-separated, as a token file does.
func (s Scope) String() string { return strings.Join(s.names(), ",") }

// parseScopes reads a comma-separated list of scope names; "all" is every v1
// scope.
func parseScopes(list string) (Scope, error) {
	var s Scope
	for name := range strings.SplitSeq(list, ",") {
		if name == scopeAllName {
			s |= ScopeAll
			continue
		}
		known := false
		for _, n := range scopeNames {
			if name == n.name {
				s |= n.scope
				known = true
			}
		}
		if !known {
			// The name is not quoted: in a line whose columns are swapped,
			// it would be the token.
			return 0, errors.New("unknown scope: want read, upload, export or all, separated by commas")
		}
	}
	return s, nil
}

const (
	// MaxTokens bounds the tokens one server takes.
	MaxTokens = 64
	// MaxLabelLength is shorter than MinTokenLength, so a token can never
	// pass as a label: a line with its columns swapped is refused rather
	// than printing the token in the log and on the console.
	MaxLabelLength = MinTokenLength - 1
	// DefaultLabel names the token of a file without a header.
	DefaultLabel = "default"
	// TokenFileHeader starts a token file that holds several tokens.
	TokenFileHeader = "#dues-tokens v1"

	tokenFileMagic = "#dues-tokens"
	commentMark    = '#'
	// DefaultScopes are what the token of a file without a header may do,
	// and every request when no token is set: what any token could do before
	// tokens had scopes.
	DefaultScopes = ScopeRead | ScopeUpload
)

// Credential is a token the server takes: the token's hash, what it may do,
// and the label the log knows it by. NewCredential makes one.
type Credential struct {
	sum    [sha256.Size]byte
	scopes Scope
	label  string
}

// Label is the name the log and the console give the credential.
func (c Credential) Label() string { return c.label }

// Scopes are what the credential may do.
func (c Credential) Scopes() Scope { return c.scopes }

// NewCredential checks a token, its scopes and its label, and keeps only the
// token's hash.
func NewCredential(token string, scopes Scope, label string) (Credential, error) {
	if err := checkToken(token); err != nil {
		return Credential{}, err
	}
	if scopes == 0 || scopes&^ScopeAll != 0 {
		return Credential{}, errors.New("the token needs one or more of the scopes read, upload and export")
	}
	if err := checkLabel(label); err != nil {
		return Credential{}, err
	}
	return Credential{sum: sha256.Sum256([]byte(token)), scopes: scopes, label: label}, nil
}

func checkLabel(l string) error {
	ok := len(l) >= 1 && len(l) <= MaxLabelLength
	for i := 0; ok && i < len(l); i++ {
		c := l[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
	}
	if !ok {
		return fmt.Errorf("the label must be 1 to %d of A-Z a-z 0-9 . _ -", MaxLabelLength)
	}
	return nil
}

// checkV1Token checks a token of a v1 file against RFC 6750's b64token, so
// that it never starts with the '#' of a comment and needs no quoting.
func checkV1Token(tok string) error {
	body := strings.TrimRight(tok, "=")
	ok := body != ""
	for i := 0; ok && i < len(body); i++ {
		c := body[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~+/", c) >= 0
	}
	if !ok {
		return errors.New("v1 tokens use the RFC 6750 token characters (A-Z a-z 0-9 - . _ ~ + /, then = only at the end); " +
			"make one with openssl rand -hex 32, or keep a file without a header")
	}
	return nil
}

// ParseTokens reads a token file. Without a header, its first line is one
// token, labelled DefaultLabel, that may read and upload, and may not start
// with '#'. A file whose first
// line is TokenFileHeader holds one token per line, as
// "<token> <scopes> <label>"; blank lines and lines starting with '#' are
// skipped. Errors name lines, never what they hold.
func ParseTokens(data []byte) ([]Credential, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if f := strings.Fields(lines[0]); len(f) > 0 && f[0] == tokenFileMagic {
		hdr := strings.Fields(TokenFileHeader)
		switch {
		case len(f) > len(hdr) && slices.Equal(f[:len(hdr)], hdr):
			return nil, fmt.Errorf("line 1: nothing may follow %q on its line", TokenFileHeader)
		case !slices.Equal(f, hdr):
			return nil, fmt.Errorf("line 1: this server reads token files that start with %q", TokenFileHeader)
		}
		return parseV1(lines)
	}
	if lines[0] == "" {
		if strings.TrimSpace(string(data)) != "" {
			return nil, fmt.Errorf("line 1 is blank: the token, or the header %q, goes on the first line", TokenFileHeader)
		}
		return nil, errors.New("the file holds no token")
	}
	// A token commented out of a file without a header would otherwise
	// still work, with one character more.
	if lines[0][0] == commentMark {
		return nil, fmt.Errorf("line 1 starts with '#': comments need the header %q on line 1; without it, line 1 is the token", TokenFileHeader)
	}
	for i, l := range lines[1:] {
		if l != "" {
			return nil, fmt.Errorf("line %d: a file without a header holds one token, on its first line; "+
				"to give several tokens, start the file with %q", i+2, TokenFileHeader)
		}
	}
	c, err := NewCredential(lines[0], DefaultScopes, DefaultLabel)
	if err != nil {
		return nil, fmt.Errorf("line 1: %w", err)
	}
	return []Credential{c}, nil
}

func parseV1(lines []string) ([]Credential, error) {
	var out []Credential
	sums := map[[sha256.Size]byte]int{}
	labels := map[string]int{}
	for i, l := range lines[1:] {
		n := i + 2
		if l == "" || l[0] == commentMark {
			continue
		}
		c, err := parseV1Line(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w (a line is <token> <scopes> <label>)", n, err)
		}
		if prev, dup := sums[c.sum]; dup {
			return nil, fmt.Errorf("lines %d and %d hold the same token", prev, n)
		}
		key := strings.ToLower(c.label)
		if prev, dup := labels[key]; dup {
			return nil, fmt.Errorf("lines %d and %d have the same label (labels are compared without regard to case)", prev, n)
		}
		if len(out) == MaxTokens {
			return nil, fmt.Errorf("line %d: a file holds at most %d tokens", n, MaxTokens)
		}
		sums[c.sum], labels[key] = n, n
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("the file holds no token")
	}
	return out, nil
}

func parseV1Line(l string) (Credential, error) {
	f := strings.Fields(l)
	if len(f) != 3 {
		return Credential{}, fmt.Errorf("it has %d columns, not 3", len(f))
	}
	if err := checkV1Token(f[0]); err != nil {
		return Credential{}, err
	}
	scopes, err := parseScopes(f[1])
	if err != nil {
		return Credential{}, err
	}
	return NewCredential(f[0], scopes, f[2])
}

// anonymous is the credential of every request when no token is set.
var anonymous = Credential{scopes: DefaultScopes}

// authenticate finds the credential a request's Authorization header names.
// Every credential is compared, in constant time; hashing first keeps the
// comparison from revealing the token's length.
func (s *Server) authenticate(header string) (Credential, bool) {
	if len(s.cfg.Tokens) == 0 {
		return anonymous, true
	}
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Credential{}, false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	found := -1
	for i := range s.cfg.Tokens {
		if subtle.ConstantTimeCompare(sum[:], s.cfg.Tokens[i].sum[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return Credential{}, false
	}
	return s.cfg.Tokens[found], true
}

// credentialOf is the credential a request was let in with; the zero
// Credential, labelled "", for a handler called directly.
func credentialOf(ctx context.Context) Credential {
	c, _ := ctx.Value(credentialKey).(Credential)
	return c
}

// authNote carries the label of a request's credential out to the access
// log, which runs outside the layer that learns it.
type authNote struct{ label string }

// logFieldBearer names a credential's label in the log. A key holding
// "token" would be redacted by the logger.
const logFieldBearer = "bearer"

func noteCredential(ctx context.Context, c Credential) {
	if n, ok := ctx.Value(authNoteKey).(*authNote); ok {
		n.label = c.label
	}
}
