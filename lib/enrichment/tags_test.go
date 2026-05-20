package enrichment

import "testing"

func TestClassifyFileAssignsExpectedTags(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		indexedType string
		wantTag     string
	}{
		{name: "image", path: "disk|||img|||photo.png", wantTag: "image"},
		{name: "document", path: "disk|||docs|||report.pdf", wantTag: "document"},
		{name: "credential", path: "disk|||secrets|||passwords.kdbx", wantTag: "credential"},
		{name: "archive", path: "disk|||tmp|||bundle.zip", wantTag: "archive"},
		{name: "script", path: "disk|||scripts|||collect.ps1", wantTag: "script"},
		{name: "browser artefact", path: "Users/A/AppData/Local/Chrome/User Data/Default/History", indexedType: "sqlite", wantTag: "browser artefact"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, tags := classifyFile(test.path, test.indexedType)
			if !hasTag(tags, test.wantTag) {
				t.Fatalf("expected tag %q in %v", test.wantTag, tags)
			}
		})
	}
}

func hasTag(tags []string, expected string) bool {
	for _, tag := range tags {
		if tag == expected {
			return true
		}
	}
	return false
}
