package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"text/tabwriter"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// The keystore's files change only under the database's lock, so every
// command that changes them opens the database writable. None recovers
// ingests: managing keys leaves the evidence alone.

// ActorCreate makes the actor name, with a key passphrase protects.
func ActorCreate(ctx context.Context, opts DBOptions, name string, passphrase []byte) (err error) {
	name, err = actorName(name)
	if err != nil {
		return err
	}
	if len(passphrase) == 0 {
		return lifecycle.Usagef("dues actor create needs the passphrase of the new key: --actor-key-password-file or --actor-key-password-stdin")
	}
	h, err := Common(ctx, opts, dbopen.ReadWrite, false)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	e, err := signer.NewKeystore(h.Root).CreateActor(name, signer.Passphrase(passphrase))
	if errors.Is(err, signer.ErrActorExists) {
		return lifecycle.Usagef("actor %q exists already; dues actor rotate gives it a new key", name)
	}
	if err != nil {
		return err
	}
	logging.GetLogger().Info("ActorCreate COMPLETE", zap.String("actor", e.Actor),
		zap.String("actor_id", signer.FormatID(e.ActorID)), zap.String("key_id", signer.FormatID(e.KeyID)))
	console.Printf("Created actor %q (actor %s, key %s)\n", e.Actor, signer.FormatID(e.ActorID), signer.FormatID(e.KeyID))
	return nil
}

// ActorRotate gives the actor name a new key: the database actor's is
// protected as the database is, any other's by passphrase. The old key
// stays in the keyring, so what it signed still verifies.
//
// It needs no old secret: a keystore is as trustworthy as custody of its
// data root, and a lost passphrase is the commonest reason to rotate.
func ActorRotate(ctx context.Context, opts DBOptions, name string, passphrase []byte) (err error) {
	name, err = signer.NormalizeName(name)
	if err != nil {
		return lifecycle.Usagef("%v", err)
	}
	if name != signer.DatabaseActor && len(passphrase) == 0 {
		return lifecycle.Usagef("dues actor rotate needs the passphrase of the new key: --actor-key-password-file or --actor-key-password-stdin")
	}
	h, err := Common(ctx, opts, dbopen.ReadWrite, false)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	var e signer.Entry
	if name == signer.DatabaseActor {
		e, err = h.RotateDatabaseActor()
	} else {
		e, err = signer.NewKeystore(h.Root).Rotate(name, signer.Passphrase(passphrase))
	}
	if errors.Is(err, signer.ErrNoActor) {
		return lifecycle.Usagef("there is no actor %q; dues actor create makes it", name)
	}
	if err != nil {
		return err
	}
	logging.GetLogger().Info("ActorRotate COMPLETE", zap.String("actor", e.Actor), zap.String("key_id", signer.FormatID(e.KeyID)))
	console.Printf("Actor %q signs with key %s from now on\n", e.Actor, signer.FormatID(e.KeyID))
	return nil
}

// ActorRevoke stops the key keyID from signing. What it signed before still
// verifies. An actor whose newest key is revoked signs again once it is
// rotated; the database actor is rotated at its next graph write.
func ActorRevoke(ctx context.Context, opts DBOptions, keyID, reason string) (err error) {
	id, err := signer.ParseID(keyID)
	if err != nil {
		return lifecycle.Usagef("%v (dues actor list shows the key IDs)", err)
	}
	h, err := Common(ctx, opts, dbopen.ReadWrite, false)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	e, err := signer.NewKeystore(h.Root).Revoke(id, reason)
	if errors.Is(err, signer.ErrNoKey) {
		return lifecycle.Usagef("the keyring holds no key %s", keyID)
	}
	if err != nil {
		return err
	}
	logging.GetLogger().Info("ActorRevoke COMPLETE", zap.String("actor", e.Actor), zap.String("key_id", keyID), zap.String("reason", reason))
	console.Printf("Revoked key %s of actor %q\n", keyID, e.Actor)
	return nil
}

// ActorList lists every key the keyring holds. It reads the keystore's
// public files only, so it needs neither the password nor the lock.
func ActorList(opts DBOptions) error {
	ks, err := keystoreOf(opts)
	if err != nil {
		return err
	}
	ring, err := ks.Keyring()
	if err != nil {
		return err
	}
	if ring.Len() == 0 {
		console.Printf("No actor in %s yet; the database actor is made the first time its graph is written\n", ks.Dir())
		return nil
	}
	tw := tabwriter.NewWriter(console.Out(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTOR\tACTOR ID\tKEY ID\tPROTECTED BY\tCREATED\tSTATUS")
	for _, e := range ring.Entries() {
		status := "retired"
		if cur, ok := ring.Current(e.ActorID); ok && cur.KeyID == e.KeyID {
			status = "current"
		}
		if e.IsRevoked() {
			status = "revoked " + e.Revoked.UTC().Format(time.RFC3339)
			if e.RevokeReason != "" {
				status += ": " + e.RevokeReason
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Actor, signer.FormatID(e.ActorID), signer.FormatID(e.KeyID),
			e.Protection, e.Created.UTC().Format(time.RFC3339), status)
	}
	return tw.Flush()
}

// ActorExportPublic writes the current public key of the actor name to out,
// or to stdout when out is empty: a key to keep outside the data root, as
// the trust anchor that what the actor signed is checked against.
func ActorExportPublic(opts DBOptions, name, out string, force bool) error {
	name, err := signer.NormalizeName(name)
	if err != nil {
		return lifecycle.Usagef("%v", err)
	}
	ks, err := keystoreOf(opts)
	if err != nil {
		return err
	}
	ring, err := ks.Keyring()
	if err != nil {
		return err
	}
	e, ok := ring.Current(signer.ActorID(name))
	switch {
	case !ok && ring.HasActor(signer.ActorID(name)):
		return fmt.Errorf("actor %q's newest key is revoked; dues actor rotate gives it a new one", name)
	case !ok && name == signer.DatabaseActor:
		return errors.New("the database actor has no key yet; it is made the first time the database's graph is written")
	case !ok:
		return lifecycle.Usagef("there is no actor %q", name)
	}
	b, err := signer.MarshalPublicKey(e)
	if err != nil {
		return err
	}
	if out == "" {
		_, err := console.Out().Write(b)
		return err
	}
	err = fsx.WriteAtomic(out, cnst.FilePerm, fsx.Options{NoOverwrite: !force, RemoveStale: true}, func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
	if errors.Is(err, fs.ErrExist) {
		return lifecycle.Usagef("%s exists; pass --force to replace it", out)
	}
	if err != nil {
		return err
	}
	console.Printf("Wrote the public key %s of actor %q to %s\n", signer.FormatID(e.KeyID), e.Actor, out)
	return nil
}

// actorName checks a name for dues actor create: the database actor is
// made by the database itself.
func actorName(name string) (string, error) {
	n, err := signer.NormalizeName(name)
	if err != nil {
		return "", lifecycle.Usagef("%v", err)
	}
	if n == signer.DatabaseActor {
		return "", lifecycle.Usagef("%q is the database's own actor, which it makes itself", n)
	}
	return n, nil
}

// keystoreOf is the keystore of the database opts names, which must exist.
func keystoreOf(opts DBOptions) (*signer.Keystore, error) {
	root, err := dbopen.ResolveRoot(opts.Path)
	if err != nil {
		return nil, err
	}
	if !dbmeta.HasDatabase(root) {
		return nil, fmt.Errorf("%w at %s", dbopen.ErrNoDatabase, root)
	}
	return signer.NewKeystore(root), nil
}
