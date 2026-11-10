package dbpackage

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"indicer/internal/duesarc"
	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/graph"
	"indicer/lib/graph/schema"
	"indicer/lib/graph/signer"
	"indicer/lib/store"
)

// maxProblems bounds the problems an error lists; Count has them all.
const maxProblems = 20

// isPackageName reports whether a name is a package's, so that name.001
// beside it is a part of a split package written before sidecars existed.
func isPackageName(p string) bool {
	_, ok := duesarc.CutSuffixFold(p, Ext)
	return ok
}

// Package is a database package opened for reading.
type Package struct {
	Path     string
	Manifest duesarc.DatabaseManifest
	// Parts is how many files the package is: 1, or a split set's parts.
	Parts int
	// Size is the package's size, all parts together.
	Size int64

	zip *duesarc.Zip
}

// Open opens a package, or a split package's parts in place, and reads its
// manifest. It reads no other entry.
func Open(path string) (*Package, error) {
	z, err := duesarc.OpenZip(path, false, isPackageName)
	if err != nil {
		return nil, err
	}
	p := &Package{Path: z.In.Path(), Parts: max(1, len(z.In.Parts())), Size: z.In.Size(), zip: z}
	if p.Manifest, err = readManifest(z.Reader); err != nil {
		z.Close()
		return nil, err
	}
	return p, nil
}

// Close closes the package's files.
func (p *Package) Close() { p.zip.Close() }

func readManifest(zr *zip.Reader) (duesarc.DatabaseManifest, error) {
	var found *zip.File
	for _, f := range zr.File {
		if f.Name == duesarc.ManifestName {
			if found != nil {
				return duesarc.DatabaseManifest{}, &DamagedError{Problems: []Problem{{Name: f.Name, Reason: "listed twice"}}, Count: 1}
			}
			found = f
		}
	}
	if found == nil {
		return duesarc.DatabaseManifest{}, errors.New("this is not a DUES package: it has no manifest.json")
	}
	if found.UncompressedSize64 > duesarc.MaxManifestBytes {
		return duesarc.DatabaseManifest{}, fmt.Errorf("the manifest is larger than %d MiB", duesarc.MaxManifestBytes>>20)
	}
	rc, err := found.Open()
	if err != nil {
		return duesarc.DatabaseManifest{}, &duesarc.DamagedError{Err: err}
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(duesarc.DamageReader{R: rc}, duesarc.MaxManifestBytes+1))
	if err != nil {
		return duesarc.DatabaseManifest{}, err
	}
	return duesarc.ParseDatabaseManifest(data)
}

// Problem is one thing wrong with a package.
type Problem struct {
	Name   string // the entry, as the zip names it
	Reason string
}

// DamagedError says what in a package does not match its manifest.
type DamagedError struct {
	Problems []Problem // the first maxProblems
	Count    int       // all of them
}

func (e *DamagedError) Error() string {
	msg := fmt.Sprintf("the package is damaged: %s: %s", e.Problems[0].Name, e.Problems[0].Reason)
	if e.Count > 1 {
		msg += fmt.Sprintf(" (and %d more)", e.Count-1)
	}
	return msg
}

func (e *DamagedError) add(name, reason string) {
	e.Count++
	if len(e.Problems) < maxProblems {
		e.Problems = append(e.Problems, Problem{Name: name, Reason: reason})
	}
}

// VerifyResult is what a verify checked.
type VerifyResult struct {
	Files int
	Bytes int64
	// SidecarChecked is set when a split package's parts were checked
	// against their sidecar.
	SidecarChecked bool
	// Evidence is what a deep verify read back from the database; nil
	// after Verify.
	Evidence *store.VerifyReport
	// Notices are one-line messages for the user from a deep verify's open.
	Notices   []string
	Signature *SignatureInfo
}

type VerifyOptions struct {
	// TrustedKey, when set, must be the key that signed the package.
	TrustedKey *signer.Entry
}

// Verify checks a package against its manifest: a split package's parts
// against their sidecar, its signature when it has one (checkSignature),
// then every entry: each file the manifest lists is there, at its size,
// with its SHA-256, and nothing else is. What does not match comes back as
// a *DamagedError, and a signature that does not hold as a
// *SignatureError.
func (p *Package) Verify(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	var res VerifyResult
	entries, bad, err := p.checkListing(ctx, &res, opts.TrustedKey)
	if err != nil {
		return res, err
	}
	return res, p.checkContents(ctx, entries, bad, &res)
}

// checkListing checks a split package's parts against their sidecar,
// matches the zip's entries to the manifest and checks the signature.
func (p *Package) checkListing(ctx context.Context, res *VerifyResult, trusted *signer.Entry) (map[string]*zip.File, *DamagedError, error) {
	if p.zip.In.Verified() {
		if err := p.zip.In.Verify(ctx); err != nil {
			return nil, nil, err
		}
		res.SidecarChecked = true
	}
	entries, bad := p.entries()
	if bad.Count == 0 {
		info, err := p.checkSignature(entries, trusted)
		if _, damaged := errors.AsType[*DamagedError](err); damaged {
			// The content check reports the keyring with every other
			// damaged entry.
			return entries, bad, nil
		}
		if err != nil {
			return nil, nil, err
		}
		res.Signature = info
	}
	return entries, bad, nil
}

// checkContents reads every entry the manifest lists and is there, checks
// it against the manifest, and adds what does not match to bad, which it
// returns when it holds anything.
func (p *Package) checkContents(ctx context.Context, entries map[string]*zip.File, bad *DamagedError, res *VerifyResult) error {
	m := p.Manifest
	ph := progress.Start(ctx, "verify", progress.Bytes, m.Bytes())
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			ph.Done(err)
			return err
		}
		zf := entries[f.Path]
		if zf == nil {
			continue
		}
		if reason, err := checkEntry(ctx, zf, f, io.Discard, ph); err != nil {
			ph.Done(err)
			return err
		} else if reason != "" {
			bad.add(zf.Name, reason)
			continue
		}
		res.Files++
		res.Bytes += f.Size
	}
	if bad.Count > 0 {
		ph.Done(bad)
		return bad
	}
	ph.Done(nil)
	return nil
}

// entries matches the zip's entries to the manifest. It returns each listed
// file's entry, by its path in the manifest, and a problem for every entry
// missing, added or listed twice.
func (p *Package) entries() (map[string]*zip.File, *DamagedError) {
	m := p.Manifest
	prefix := duesarc.DatabaseDir + "/"
	files := map[string]bool{}
	for _, f := range m.Files {
		files[f.Path] = true
	}
	dirs := map[string]bool{}
	for _, d := range m.Dirs {
		dirs[prefix+d+"/"] = true
	}
	bad := &DamagedError{}
	seen := map[string]bool{}
	listed := map[string]*zip.File{}
	for _, zf := range p.zip.File {
		name := zf.Name
		if seen[name] {
			bad.add(name, "listed twice")
			continue
		}
		seen[name] = true
		rel, inDB := strings.CutPrefix(name, prefix)
		switch {
		case name == duesarc.ManifestName:
		case dirs[name]:
		case strings.HasSuffix(name, "/"):
			bad.add(name, "a directory the manifest does not list")
		case !inDB || !files[rel]:
			bad.add(name, "a file the manifest does not list")
		case isPrivateKey(rel):
			bad.add(name, "an actor's private key, which a package never carries")
		default:
			listed[rel] = zf
		}
	}
	for _, f := range m.Files {
		if listed[f.Path] == nil {
			bad.add(prefix+f.Path, "missing")
		}
	}
	for _, d := range m.Dirs {
		if !seen[prefix+d+"/"] {
			bad.add(prefix+d+"/", "missing")
		}
	}
	checkGraphImage(listed, bad)
	return listed, bad
}

func isPrivateKey(rel string) bool {
	return strings.HasPrefix(rel, cnst.KEYSDIR+"/") && strings.HasSuffix(rel, signer.KeyFileExt)
}

// checkGraphImage adds a problem when the graph's schema marker records a
// compacted image and the package holds none: its log holds only what came
// after the compaction, so the graph would open as a smaller one. A marker
// that cannot be read is left to the content check.
func checkGraphImage(listed map[string]*zip.File, bad *DamagedError) {
	zf := listed[cnst.GRAPHDIR+"/"+schema.MarkerFileName]
	image := cnst.GRAPHDIR + "/" + graph.ImageFileName
	if zf == nil || listed[image] != nil {
		return
	}
	rc, err := zf.Open()
	if err != nil {
		return
	}
	defer rc.Close()
	var marker struct {
		Image json.RawMessage `json:"image"`
	}
	data, err := io.ReadAll(io.LimitReader(rc, duesarc.MaxManifestBytes))
	if err != nil || json.Unmarshal(data, &marker) != nil {
		return
	}
	if len(marker.Image) > 0 && string(marker.Image) != "null" {
		bad.add(duesarc.DatabaseDir+"/"+image, "missing, and the graph's schema marker records a compacted image")
	}
}

// checkEntry copies one entry to w and says what does not match the
// manifest, or "" when it all does. Only a failure to go on (cancellation,
// or writing w) is an error; damage is a reason. It reads at most one byte
// past the size the manifest gives.
func checkEntry(ctx context.Context, zf *zip.File, want duesarc.File, w io.Writer, ph *progress.Phase) (string, error) {
	if zf.UncompressedSize64 != uint64(want.Size) {
		return fmt.Sprintf("%d bytes; the manifest says %d", zf.UncompressedSize64, want.Size), nil
	}
	rc, err := zf.Open()
	if err != nil {
		return "unreadable: " + err.Error(), nil
	}
	defer rc.Close()
	h := sha256.New()
	n, err := duesarc.Copy(ctx, io.MultiWriter(w, h), duesarc.DamageReader{R: io.LimitReader(rc, want.Size+1)}, ph.Add)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		if _, damaged := errors.AsType[*duesarc.DamagedError](err); damaged {
			return "unreadable: " + errors.Unwrap(err).Error(), nil
		}
		return "", err
	case n != want.Size:
		return fmt.Sprintf("%d bytes; the manifest says %d", n, want.Size), nil
	case hex.EncodeToString(h.Sum(nil)) != want.SHA256:
		return "its SHA-256 does not match the manifest", nil
	}
	return "", nil
}
