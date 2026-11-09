package dbopen

import (
	"bytes"
	"errors"
	"fmt"

	"indicer/internal/console"
	"indicer/lib/dbmeta"
	"indicer/lib/graph"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// Actor names who signs the graph commits a writable open makes. A nil
// Request.Actor is the database's own actor, signer.DatabaseActor.
type Actor struct {
	Name string
	// Passphrase unlocks the actor's key. The open keeps a copy until it
	// closes.
	Passphrase []byte
}

// startSigning has the graph handles this database's writable open makes
// sign their commits as its actor (graph.SignWith). The actor is resolved
// at the first of them, so a database whose graph is never written gets no
// key.
func (h *Handle) startSigning(req Request, keys dbmeta.Keys) error {
	// Kept for read-only opens too: a package of the database is signed by
	// its actor (Signer).
	h.actorKey = bytes.Clone(keys.Actor)
	if a := req.Actor; a != nil {
		name, err := signer.NormalizeName(a.Name)
		if err != nil {
			return err
		}
		if name == signer.DatabaseActor {
			return fmt.Errorf("actor %q is the database's own; it signs when no other actor is named", name)
		}
		h.actor = &Actor{Name: name, Passphrase: bytes.Clone(a.Passphrase)}
	}
	if h.ReadOnly {
		return nil
	}
	remove, err := graph.SignWith(h.Root, h.Signer)
	if err != nil {
		return err
	}
	h.unsign = remove
	return nil
}

// Signer is the provider that signs this database's graph commits and
// packages: the actor Request.Actor named, or else the database's own. A
// writable handle makes the database actor the first time it is needed; a
// read-only one resolves an actor that exists and makes none. The handle
// closes the provider when it closes.
func (h *Handle) Signer() (signer.Provider, error) {
	h.signMu.Lock()
	defer h.signMu.Unlock()
	if h.provider != nil {
		return h.provider, nil
	}
	if h.DB == nil {
		return nil, errors.New("the database is closed")
	}
	p, err := h.resolveSigner()
	if err != nil {
		return nil, err
	}
	h.provider = p
	ref := p.Actor()
	logging.GetLogger().Info("signer resolved", zap.String("root", h.Root), zap.String("actor", ref.Name),
		zap.String("actor_id", signer.FormatID(ref.ActorID)), zap.String("key_id", signer.FormatID(ref.KeyID)))
	return p, nil
}

func (h *Handle) resolveSigner() (signer.Provider, error) {
	ks := signer.NewKeystore(h.Root)
	if a := h.actor; a != nil {
		p, err := ks.Resolve(a.Name, signer.Passphrase(a.Passphrase))
		if errors.Is(err, signer.ErrNoActor) {
			return nil, fmt.Errorf("actor %q is not in %s (dues actor create makes it): %w", a.Name, ks.Dir(), err)
		}
		return p, err
	}
	u := signer.ForDatabase(h.actorKey)
	p, err := ks.Resolve(signer.DatabaseActor, u)
	var protected *signer.ProtectionError
	switch {
	case err == nil:
		return p, nil
	case h.ReadOnly:
		return nil, fmt.Errorf("the database actor of %s: %w", h.Root, err)
	case errors.Is(err, signer.ErrNoActor):
		_, err = ks.CreateActor(signer.DatabaseActor, u)
	case errors.Is(err, signer.ErrNoKey):
		// Its newest key is revoked, or the key file is not here: the
		// keyring a package brings holds its database's public keys, never
		// a private one.
		_, err = ks.Rotate(signer.DatabaseActor, u)
	case errors.Is(err, signer.ErrWrongPassphrase), errors.As(err, &protected):
		// The database has opened, so its key is the right one: the
		// actor's key is an earlier database's, one reset or replaced at
		// this root. The keyring keeps that key for the commits it signed.
		msg := fmt.Sprintf("the database actor's key in %s belongs to an earlier database at %s; a new one signs from now on", ks.Dir(), h.Root)
		logging.GetLogger().Warn("signer: database actor key replaced", zap.String("root", h.Root), zap.Error(err))
		console.Errorln("warning: " + msg)
		_, err = ks.Rotate(signer.DatabaseActor, u)
	default:
		return nil, fmt.Errorf("the database actor of %s: %w", h.Root, err)
	}
	if err != nil {
		return nil, fmt.Errorf("making a key for the database actor of %s: %w", h.Root, err)
	}
	return ks.Resolve(signer.DatabaseActor, u)
}

// RotateDatabaseActor gives the database actor a new key, protected as the
// database's keys are, and keeps the old one in the keyring for what it
// signed. A database actor not made yet is made.
func (h *Handle) RotateDatabaseActor() (signer.Entry, error) {
	if h.ReadOnly || h.DB == nil {
		return signer.Entry{}, errors.New("rotating the database actor's key needs the database open for writing")
	}
	h.signMu.Lock()
	defer h.signMu.Unlock()
	ks := signer.NewKeystore(h.Root)
	u := signer.ForDatabase(h.actorKey)
	e, err := ks.Rotate(signer.DatabaseActor, u)
	if errors.Is(err, signer.ErrNoActor) {
		return ks.CreateActor(signer.DatabaseActor, u)
	}
	return e, err
}

// stopSigning undoes startSigning and closes the provider.
func (h *Handle) stopSigning() {
	if h.unsign != nil {
		h.unsign()
		h.unsign = nil
	}
	h.signMu.Lock()
	defer h.signMu.Unlock()
	if h.provider != nil {
		_ = h.provider.Close()
		h.provider = nil
	}
	clear(h.actorKey)
	h.actorKey = nil
	if h.actor != nil {
		clear(h.actor.Passphrase)
		h.actor = nil
	}
}
