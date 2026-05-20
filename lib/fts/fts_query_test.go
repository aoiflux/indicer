package fts

import "testing"

func TestBuildParsedQueryStringAND(t *testing.T) {
	got := buildParsedQueryString([]string{"foo", "bar baz"}, QueryModeAnd)
	want := "foo AND \"bar baz\""
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBuildParsedQueryStringOR(t *testing.T) {
	got := buildParsedQueryString([]string{"foo", "bar"}, QueryModeOr)
	want := "foo OR bar"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBuildParsedQueryStringEscapesSpecialChars(t *testing.T) {
	got := buildParsedQueryString([]string{"foo:bar", `alpha "beta"`}, QueryModeAnd)
	want := `foo\:bar AND "alpha \"beta\""`
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestBuildParsedQueryStringSkipsEmptyTerms(t *testing.T) {
	got := buildParsedQueryString([]string{" ", "foo"}, QueryModeAnd)
	if got != "foo" {
		t.Fatalf("expected %q, got %q", "foo", got)
	}
}
