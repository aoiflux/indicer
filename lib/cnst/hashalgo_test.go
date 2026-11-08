package cnst

import "testing"

func TestNormalizeHashAlgo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default empty", in: "", want: BLAKE3},
		{name: "sha3 lower", in: "sha3", want: SHA3},
		{name: "blake3 upper trimmed", in: "  BLAKE3 ", want: BLAKE3},
		{name: "invalid", in: "md5", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeHashAlgo(tt.in); got != tt.want {
				t.Fatalf("NormalizeHashAlgo(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseHashAlgo(t *testing.T) {
	if got, err := ParseHashAlgo("sha3"); err != nil || got != SHA3 {
		t.Fatalf("ParseHashAlgo(sha3) = %q, %v; want %q", got, err, SHA3)
	}
	if _, err := ParseHashAlgo("md5"); err == nil {
		t.Fatal("ParseHashAlgo accepted an invalid algorithm")
	}
}

func TestNormalizeHashStrategy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default empty", in: "", want: SyncHashStrategy},
		{name: "sync lower", in: "sync", want: SyncHashStrategy},
		{name: "async upper trimmed", in: "  ASYNCHASH ", want: AsyncHashStrategy},
		{name: "hocho lower", in: "hocho", want: HochoHashStrategy},
		{name: "invalid", in: "chunkhash", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeHashStrategy(tt.in); got != tt.want {
				t.Fatalf("NormalizeHashStrategy(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseHashStrategy(t *testing.T) {
	if got, err := ParseHashStrategy("async"); err != nil || got != AsyncHashStrategy {
		t.Fatalf("ParseHashStrategy(async) = %q, %v; want %q", got, err, AsyncHashStrategy)
	}
	if _, err := ParseHashStrategy("merkle"); err == nil {
		t.Fatal("ParseHashStrategy accepted an invalid strategy")
	}
}
