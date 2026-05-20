package util

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"testing"
)

func TestSealAESUsesRandomNonce(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	plaintext := []byte("forensic payload")

	sealedOne, err := SealAES(key, plaintext)
	if err != nil {
		t.Fatalf("SealAES first call: %v", err)
	}
	sealedTwo, err := SealAES(key, plaintext)
	if err != nil {
		t.Fatalf("SealAES second call: %v", err)
	}
	if bytes.Equal(sealedOne, sealedTwo) {
		t.Fatal("SealAES produced identical ciphertext for repeated input")
	}

	openedOne, err := UnsealAES(key, sealedOne)
	if err != nil {
		t.Fatalf("UnsealAES first ciphertext: %v", err)
	}
	openedTwo, err := UnsealAES(key, sealedTwo)
	if err != nil {
		t.Fatalf("UnsealAES second ciphertext: %v", err)
	}
	if !bytes.Equal(openedOne, plaintext) {
		t.Fatalf("first plaintext mismatch: got %q want %q", openedOne, plaintext)
	}
	if !bytes.Equal(openedTwo, plaintext) {
		t.Fatalf("second plaintext mismatch: got %q want %q", openedTwo, plaintext)
	}
}

func TestUnsealAESReadsLegacyCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{2}, 32)
	plaintext := []byte("legacy payload")

	legacyCiphertext := sealAESLegacy(t, key, plaintext)
	opened, err := UnsealAES(key, legacyCiphertext)
	if err != nil {
		t.Fatalf("UnsealAES legacy ciphertext: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("legacy plaintext mismatch: got %q want %q", opened, plaintext)
	}
}

func TestUnsealAESRejectsMalformedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	if _, err := UnsealAES(key, []byte("not-valid-ciphertext")); err == nil {
		t.Fatal("UnsealAES accepted malformed ciphertext")
	}
}

func sealAESLegacy(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	nonce := sha256.Sum256(key)
	return gcm.Seal(nil, nonce[:gcm.NonceSize()], plaintext, nil)
}
