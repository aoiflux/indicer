package dbpackage

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"indicer/internal/duesarc"
	"indicer/lib/cnst"
	"indicer/lib/graph/signer"
)

const keyringPath = cnst.KEYSDIR + "/" + signer.KeyringName

// SignatureInfo is who signed a package whose signature holds.
type SignatureInfo struct {
	Actor      string
	ActorID    uint64
	KeyID      uint64
	Protection signer.Protection
	Trusted    bool
}

type SignatureError struct {
	Reason string
}

func (e *SignatureError) Error() string { return "the package's signature does not hold: " + e.Reason }

func badSignature(format string, a ...any) error {
	return &SignatureError{Reason: fmt.Sprintf(format, a...)}
}

func sign(m *duesarc.DatabaseManifest, p signer.Provider) error {
	ref := p.Actor()
	m.Signature = &duesarc.Signature{
		Scheme:     duesarc.SignatureScheme,
		Actor:      ref.Name,
		ActorID:    signer.FormatID(ref.ActorID),
		KeyID:      signer.FormatID(ref.KeyID),
		PublicKey:  p.Public(),
		Protection: p.Protection().String(),
	}
	sig, err := p.Signer().Sign(m.Statement())
	if err != nil {
		m.Signature = nil
		return fmt.Errorf("signing the package as actor %q: %w", ref.Name, err)
	}
	m.Signature.Sig = sig
	return nil
}

// checkSignature returns nil info for an unsigned package when no trusted
// key is given. Without a trusted key a valid signature proves only that the
// package is unchanged since signing: whoever changes it can sign it again
// with a key and keyring of their own.
func (p *Package) checkSignature(entries map[string]*zip.File, trusted *signer.Entry) (*SignatureInfo, error) {
	m := p.Manifest
	s := m.Signature
	if s == nil {
		if trusted != nil {
			return nil, &SignatureError{Reason: "the package is unsigned, and a trusted key asks for a signature"}
		}
		return nil, nil
	}
	info, err := checkIdentity(s)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(s.PublicKey, m.Statement(), s.Sig) {
		return nil, badSignature("it does not verify under key %s of actor %q: the manifest changed after it was signed", s.KeyID, s.Actor)
	}
	ring, err := p.packagedKeyring(entries)
	if err != nil {
		return nil, err
	}
	e, ok := ring.Key(info.KeyID)
	switch {
	case !ok:
		return nil, badSignature("key %s of actor %q, which signed it, is not in the package's keyring", s.KeyID, s.Actor)
	case e.IsRevoked():
		return nil, badSignature("key %s of actor %q, which signed it, is revoked in the package's keyring (%s)", s.KeyID, s.Actor, revocation(e))
	case e.Protection != info.Protection:
		return nil, badSignature("the signature says key %s is protected by %s; the package's keyring says %s", s.KeyID, info.Protection, e.Protection)
	}
	if trusted != nil {
		switch {
		case trusted.KeyID == info.KeyID:
			info.Trusted = true
		case trusted.ActorID == info.ActorID:
			return nil, badSignature("key %s of actor %q signed it; the trusted key is that actor's key %s (after a rotation, export its public key again)",
				s.KeyID, s.Actor, signer.FormatID(trusted.KeyID))
		default:
			return nil, badSignature("actor %q signed it; the trusted key is actor %q's", s.Actor, trusted.Actor)
		}
	}
	return info, nil
}

func checkIdentity(s *duesarc.Signature) (*SignatureInfo, error) {
	if s.Scheme != duesarc.SignatureScheme {
		return nil, badSignature("its scheme is %q, and this DUES checks only %q", s.Scheme, duesarc.SignatureScheme)
	}
	if len(s.PublicKey) != ed25519.PublicKeySize || len(s.Sig) != ed25519.SignatureSize {
		return nil, badSignature("its key or signature is not Ed25519's size")
	}
	name, err := signer.NormalizeName(s.Actor)
	if err != nil || name != s.Actor {
		return nil, badSignature("its actor name %q is not in canonical form", s.Actor)
	}
	actorID, err := signer.ParseID(s.ActorID)
	if err != nil || actorID != signer.ActorID(name) {
		return nil, badSignature("its actor ID %q is not the ID of actor %q", s.ActorID, name)
	}
	keyID, err := signer.ParseID(s.KeyID)
	if err != nil || keyID != signer.KeyID(s.PublicKey) {
		return nil, badSignature("its key ID %q is not the ID of its public key", s.KeyID)
	}
	prot, err := signer.ParseProtection(s.Protection)
	if err != nil {
		return nil, badSignature("%v", err)
	}
	return &SignatureInfo{Actor: name, ActorID: actorID, KeyID: keyID, Protection: prot}, nil
}

// packagedKeyring checks the keyring against the manifest itself: the
// content check runs after the signature check.
func (p *Package) packagedKeyring(entries map[string]*zip.File) (*signer.Keyring, error) {
	zf := entries[keyringPath]
	var want *duesarc.File
	for i := range p.Manifest.Files {
		if p.Manifest.Files[i].Path == keyringPath {
			want = &p.Manifest.Files[i]
		}
	}
	if zf == nil || want == nil {
		return nil, badSignature("the package holds no keyring (%s/%s) to check the signing key against", duesarc.DatabaseDir, keyringPath)
	}
	if want.Size > signer.MaxJSONLen {
		return nil, badSignature("its keyring is larger than %d MiB", signer.MaxJSONLen/cnst.MB)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, &DamagedError{Problems: []Problem{{Name: zf.Name, Reason: "unreadable: " + err.Error()}}, Count: 1}
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, want.Size+1))
	sum := sha256.Sum256(data)
	if err != nil || int64(len(data)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
		return nil, &DamagedError{Problems: []Problem{{Name: zf.Name, Reason: "does not match the manifest"}}, Count: 1}
	}
	ring, err := signer.ParseKeyring(data)
	if err != nil {
		return nil, badSignature("its keyring: %v", err)
	}
	return ring, nil
}

func revocation(e signer.Entry) string {
	s := "revoked " + e.Revoked.UTC().Format(time.RFC3339)
	if e.RevokeReason != "" {
		s += ": " + e.RevokeReason
	}
	return s
}
