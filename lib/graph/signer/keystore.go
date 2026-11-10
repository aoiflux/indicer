package signer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/fsx"
)

// Keystore is the dues.keys directory of a data root:
//
//	keyring.json                  the public keys (Keyring)
//	actors.json                   the actors' names and IDs
//	actors/<actor>-<key>.key      each key's protected seed
//
// Every file is replaced atomically, so a crash leaves it as it was or
// complete. Writes add in an order a crash cannot confuse: the actor's name,
// then its key file, then the keyring entry that makes the key current. The
// caller holds the data root's write lock, as two writers would each replace
// the other's keyring.
type Keystore struct {
	dir string
	now func() time.Time
	// written, when set, runs once a JSON file's new content is in its
	// temporary file, before the rename. Crash tests stop the process there.
	written func(path string)
}

// Errors from the keystore.
var (
	ErrNoActor     = errors.New("no such actor")
	ErrActorExists = errors.New("the actor already exists")
	ErrNoKey       = errors.New("no usable key")
)

const (
	KeyringName = "keyring.json"
	actorsName  = "actors.json"
	keysDirName = "actors"
	// KeyFileExt ends the name of every key file, which holds a private key:
	// what copies a data root elsewhere leaves these out.
	KeyFileExt = ".key"
	// keyPerm keeps a key file to its owner; it is all that protects an
	// unprotected key. On Windows the file inherits the folder's access list.
	keyPerm = 0o600
	// MaxJSONLen bounds what is read of keyring.json or actors.json.
	MaxJSONLen = 4 << 20
)

// NewKeystore returns the keystore of the data root root. It reads and
// creates nothing until asked.
func NewKeystore(root string) *Keystore {
	return &Keystore{dir: filepath.Join(root, cnst.KEYSDIR), now: time.Now}
}

// Dir is the keystore's directory.
func (k *Keystore) Dir() string { return k.dir }

func (k *Keystore) keyringPath() string { return filepath.Join(k.dir, KeyringName) }
func (k *Keystore) actorsPath() string  { return filepath.Join(k.dir, actorsName) }

// KeyPath is where the key file of key keyID of actor actorID lives.
func (k *Keystore) KeyPath(actorID, keyID uint64) string {
	return filepath.Join(k.dir, keysDirName, FormatID(actorID)+"-"+FormatID(keyID)+KeyFileExt)
}

// Keyring reads the keyring; a keystore without one has an empty keyring.
func (k *Keystore) Keyring() (*Keyring, error) {
	b, err := readLimited(k.keyringPath(), MaxJSONLen)
	if errors.Is(err, fs.ErrNotExist) {
		return &Keyring{}, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := ParseKeyring(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.keyringPath(), err)
	}
	return r, nil
}

// Actor is one name in actors.json.
type Actor struct {
	Name    string
	ID      uint64
	Created time.Time
}

const actorsVersion = 1

type actorsFile struct {
	Version int         `json:"version"`
	Actors  []actorJSON `json:"actors"`
}

type actorJSON struct {
	Name    string    `json:"name"`
	ActorID string    `json:"actor_id"`
	Created time.Time `json:"created_at"`
}

// Actors reads the actors, in the order they were added; none without an
// actors.json.
func (k *Keystore) Actors() ([]Actor, error) {
	b, err := readLimited(k.actorsPath(), MaxJSONLen)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	actors, err := parseActors(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.actorsPath(), err)
	}
	return actors, nil
}

// parseActors reads an actors.json: every name canonical, every ID its
// name's, no name twice.
func parseActors(b []byte) ([]Actor, error) {
	var f actorsFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("actors: %w", err)
	}
	if dec.More() {
		return nil, errors.New("actors: data after the table")
	}
	if f.Version != actorsVersion {
		return nil, fmt.Errorf("actors version %d is not one this build reads", f.Version)
	}
	if len(f.Actors) > maxKeys {
		return nil, fmt.Errorf("actors table holds %d actors, more than %d", len(f.Actors), maxKeys)
	}
	actors := make([]Actor, 0, len(f.Actors))
	for i, j := range f.Actors {
		name, err := NormalizeName(j.Name)
		if err != nil {
			return nil, fmt.Errorf("actor %d: %w", i, err)
		}
		if name != j.Name {
			return nil, fmt.Errorf("actor %d: name %q is not in canonical form", i, j.Name)
		}
		id, err := ParseID(j.ActorID)
		if err != nil {
			return nil, fmt.Errorf("actor %d: %w", i, err)
		}
		if id != ActorID(name) {
			return nil, fmt.Errorf("actor %q: ID %s is not its name's", name, j.ActorID)
		}
		if slices.ContainsFunc(actors, func(a Actor) bool { return a.Name == name }) {
			return nil, fmt.Errorf("actor %q appears twice", name)
		}
		actors = append(actors, Actor{Name: name, ID: id, Created: j.Created})
	}
	return actors, nil
}

func marshalActors(actors []Actor) ([]byte, error) {
	f := actorsFile{Version: actorsVersion, Actors: make([]actorJSON, 0, len(actors))}
	for _, a := range actors {
		f.Actors = append(f.Actors, actorJSON{Name: a.Name, ActorID: FormatID(a.ID), Created: a.Created})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CreateActor adds the actor called name with a new key protected as u says,
// and returns the key's entry. A name another actor's ID collides with is
// refused; so is an actor that already has a key. An actor a crash left with
// a name and no key gets one.
func (k *Keystore) CreateActor(name string, u Unlock) (Entry, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Entry{}, err
	}
	actors, err := k.Actors()
	if err != nil {
		return Entry{}, err
	}
	ring, err := k.Keyring()
	if err != nil {
		return Entry{}, err
	}
	id := ActorID(name)
	registered, err := registration(actors, name, id)
	if err != nil {
		return Entry{}, err
	}
	if ring.HasActor(id) {
		return Entry{}, fmt.Errorf("%w: %q", ErrActorExists, name)
	}
	if !registered {
		actors = append(actors, Actor{Name: name, ID: id, Created: k.now().UTC()})
		b, err := marshalActors(actors)
		if err != nil {
			return Entry{}, err
		}
		if err := k.writeJSON(k.actorsPath(), b); err != nil {
			return Entry{}, err
		}
	}
	return k.addKey(ring, name, id, u)
}

// registration reports whether actors already holds name, whose ID is id. A
// different name with the same ID is a collision, refused.
func registration(actors []Actor, name string, id uint64) (bool, error) {
	registered := false
	for _, a := range actors {
		switch {
		case a.Name == name:
			registered = true
		case a.ID == id:
			return false, fmt.Errorf("actor name %q has the same ID (%s) as actor %q; choose another name", name, FormatID(id), a.Name)
		}
	}
	return registered, nil
}

// Rotate gives the existing actor called name a new key, protected as u
// says, and returns its entry. The actor's earlier keys stay in the keyring,
// so what they signed still verifies.
func (k *Keystore) Rotate(name string, u Unlock) (Entry, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Entry{}, err
	}
	actors, err := k.Actors()
	if err != nil {
		return Entry{}, err
	}
	if !slices.ContainsFunc(actors, func(a Actor) bool { return a.Name == name }) {
		return Entry{}, fmt.Errorf("%w: %q", ErrNoActor, name)
	}
	ring, err := k.Keyring()
	if err != nil {
		return Entry{}, err
	}
	return k.addKey(ring, name, ActorID(name), u)
}

// Revoke stops the key with ID keyID from signing, for reason, and returns
// its entry. Its signatures still verify. An actor whose current key is
// revoked cannot sign until it rotates.
func (k *Keystore) Revoke(keyID uint64, reason string) (Entry, error) {
	ring, err := k.Keyring()
	if err != nil {
		return Entry{}, err
	}
	if err := ring.revoke(keyID, k.now().UTC(), reason); err != nil {
		return Entry{}, err
	}
	if err := k.writeKeyring(ring); err != nil {
		return Entry{}, err
	}
	e, _ := ring.Key(keyID)
	return e, nil
}

// addKey makes a new key for the actor, writes its key file, then adds it to
// the keyring, where it becomes the actor's current key.
func (k *Keystore) addKey(ring *Keyring, name string, actorID uint64, u Unlock) (Entry, error) {
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	var keyID uint64
	for range 4 {
		p, s, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return Entry{}, err
		}
		if _, dup := ring.Key(KeyID(p)); !dup {
			pub, priv, keyID = p, s, KeyID(p)
			break
		}
		clear(s)
	}
	if priv == nil {
		return Entry{}, errors.New("every new key collided with a key in the keyring")
	}
	defer clear(priv)
	seed := priv.Seed()
	defer clear(seed)
	file, err := sealKey(seed, keyID, actorID, u)
	if err != nil {
		return Entry{}, err
	}
	path := k.KeyPath(actorID, keyID)
	if err := os.MkdirAll(filepath.Dir(path), cnst.DirPerm); err != nil {
		return Entry{}, err
	}
	err = fsx.WriteAtomic(path, keyPerm, fsx.Options{NoOverwrite: true}, func(f *os.File) error {
		_, err := f.Write(file)
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e := Entry{KeyID: keyID, ActorID: actorID, Actor: name, Public: pub, Protection: u.protection, Created: k.now().UTC()}
	if err := ring.add(e); err != nil {
		return Entry{}, errors.Join(err, fsx.Remove(path))
	}
	if err := k.writeKeyring(ring); err != nil {
		return Entry{}, errors.Join(err, fsx.Remove(path))
	}
	return e, nil
}

func (k *Keystore) writeKeyring(r *Keyring) error {
	b, err := r.Marshal()
	if err != nil {
		return err
	}
	return k.writeJSON(k.keyringPath(), b)
}

// writeJSON replaces the public file path with b.
func (k *Keystore) writeJSON(path string, b []byte) error {
	if err := os.MkdirAll(k.dir, cnst.DirPerm); err != nil {
		return err
	}
	return fsx.WriteAtomic(path, cnst.FilePerm, fsx.Options{RemoveStale: true}, func(f *os.File) error {
		if _, err := f.Write(b); err != nil {
			return err
		}
		if k.written != nil {
			k.written(path)
		}
		return nil
	})
}

// Resolve opens the current key of the actor called name with u and checks
// it: the seed must be the keyring's public key, and sign a test message
// with a 64-byte signature that verifies. The provider verifies against the
// keyring as it is now.
func (k *Keystore) Resolve(name string, u Unlock) (Provider, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	ring, err := k.Keyring()
	if err != nil {
		return nil, err
	}
	actorID := ActorID(name)
	e, ok := ring.Current(actorID)
	switch {
	case !ok && ring.HasActor(actorID):
		return nil, fmt.Errorf("%w: every key of actor %q is revoked", ErrNoKey, name)
	case !ok:
		return nil, fmt.Errorf("%w: %q", ErrNoActor, name)
	}
	path := k.KeyPath(e.ActorID, e.KeyID)
	b, err := readLimited(path, int64(maxKeyFileLen))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: the key file of actor %q's key %s is not in this keystore", ErrNoKey, name, FormatID(e.KeyID))
	}
	if err != nil {
		return nil, err
	}
	f, err := parseKeyFile(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.keyID != e.KeyID || f.actorID != e.ActorID {
		return nil, fmt.Errorf("%s is labelled key %s of actor %s, not key %s of actor %s", path,
			FormatID(f.keyID), FormatID(f.actorID), FormatID(e.KeyID), FormatID(e.ActorID))
	}
	seed, err := f.open(u)
	if err != nil {
		return nil, fmt.Errorf("actor %q: %w", name, err)
	}
	defer clear(seed)
	key := &Key{entry: e, priv: ed25519.NewKeyFromSeed(seed)}
	if err := key.selfTest(); err != nil {
		key.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &provider{key: key, verifier: ring.Verifier()}, nil
}

// readLimited reads path, refusing a file longer than limit.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is longer than the %d bytes it may hold", path, limit)
	}
	return b, nil
}
