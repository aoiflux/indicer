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

func TestSetHashAlgo(t *testing.T) {
	original := HASHALGO
	t.Cleanup(func() {
		HASHALGO = original
	})

	if err := SetHashAlgo("sha3"); err != nil {
		t.Fatalf("SetHashAlgo returned unexpected error: %v", err)
	}
	if HASHALGO != SHA3 {
		t.Fatalf("HASHALGO = %q, want %q", HASHALGO, SHA3)
	}

	if err := SetHashAlgo("md5"); err == nil {
		t.Fatal("SetHashAlgo accepted an invalid algorithm")
	}
	if HASHALGO != SHA3 {
		t.Fatalf("HASHALGO changed after invalid input: got %q, want %q", HASHALGO, SHA3)
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

func TestSetStoreHashStrategy(t *testing.T) {
	original := StoreHashStrategy
	t.Cleanup(func() {
		StoreHashStrategy = original
	})

	if err := SetStoreHashStrategy("async"); err != nil {
		t.Fatalf("SetStoreHashStrategy returned unexpected error: %v", err)
	}
	if StoreHashStrategy != AsyncHashStrategy {
		t.Fatalf("StoreHashStrategy = %q, want %q", StoreHashStrategy, AsyncHashStrategy)
	}

	if err := SetStoreHashStrategy("merkle"); err == nil {
		t.Fatal("SetStoreHashStrategy accepted an invalid strategy")
	}
	if StoreHashStrategy != AsyncHashStrategy {
		t.Fatalf("StoreHashStrategy changed after invalid input: got %q, want %q", StoreHashStrategy, AsyncHashStrategy)
	}
}
