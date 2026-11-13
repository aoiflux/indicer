package api

import (
	"slices"
	"strings"
	"testing"

	"indicer/lib/server"
)

// The startup lines name each token by its label and scopes, never by the
// token itself.
func TestTokenLines(t *testing.T) {
	t.Parallel()
	if got, want := tokenLines(nil), []string{"No token: every request may read,upload; export is off"}; !slices.Equal(got, want) {
		t.Errorf("no token: %q, want %q", got, want)
	}
	reader, admin := strings.Repeat("r", 40), strings.Repeat("a", 40)
	var creds []server.Credential
	for _, c := range []struct {
		token  string
		scopes server.Scope
		label  string
	}{{reader, server.ScopeRead, "reader"}, {admin, server.ScopeAll, "admin"}} {
		cred, err := server.NewCredential(c.token, c.scopes, c.label)
		if err != nil {
			t.Fatal(err)
		}
		creds = append(creds, cred)
	}
	got := tokenLines(creds)
	want := []string{
		"Bearer token required on every request except /healthz and /readyz",
		"Token reader: read",
		"Token admin: read,upload,export",
	}
	if !slices.Equal(got, want) {
		t.Errorf("tokens: %q, want %q", got, want)
	}
	for _, l := range got {
		if strings.Contains(l, reader) || strings.Contains(l, admin) {
			t.Errorf("a line holds a token: %q", l)
		}
	}
}
