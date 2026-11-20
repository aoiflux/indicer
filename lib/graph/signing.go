package graph

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"indicer/internal/console"
	"indicer/lib/graph/schema"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"
	"indicer/lib/util"

	graphenedisk "github.com/aoiflux/graphene/disk"
	"github.com/aoiflux/graphene/signing"
	"go.uber.org/zap"
)

var (
	signMu sync.Mutex
	// signings holds, per graph directory, what resolves the actor its
	// writable handles sign with.
	signings = map[string]func() (signer.Provider, error){}
)

// SignWith has every writable handle opened on root's graph from now on sign
// its commits with the provider resolve returns, until remove is called.
// resolve runs at each such open, so an actor is made only for a database
// that writes its graph; the caller keeps what it resolves and closes it.
//
// graphene signs per open, not per commit, so a process has one actor per
// graph: a second SignWith of the same root is refused, and so is one while
// the graph is open.
func SignWith(root string, resolve func() (signer.Provider, error)) (remove func(), err error) {
	dir := util.GraphPath(root)
	key, err := registryKey(dir)
	if err != nil {
		return nil, fmt.Errorf("graph: resolve %s: %w", dir, err)
	}
	signMu.Lock()
	defer signMu.Unlock()
	if _, ok := signings[key]; ok {
		return nil, fmt.Errorf("graph: %s already has an actor signing its commits in this process", dir)
	}
	registryMu.Lock()
	_, open := registry[key]
	registryMu.Unlock()
	if open {
		return nil, fmt.Errorf("graph: %s is open already; the actor that signs it is set before it opens", dir)
	}
	signings[key] = resolve
	return sync.OnceFunc(func() {
		signMu.Lock()
		delete(signings, key)
		signMu.Unlock()
	}), nil
}

func signingFor(key string) func() (signer.Provider, error) {
	signMu.Lock()
	defer signMu.Unlock()
	return signings[key]
}

// commitVerifier checks a log's commit signatures against the keyring. A
// commit signed by a key the keyring does not hold cannot be checked: it is
// counted and let through, unless the store requires signed commits. A
// signature that does not verify fails the open either way.
type commitVerifier struct {
	keys     *signer.Verifier
	required bool
	unknown  atomic.Int64
}

func (v *commitVerifier) Verify(keyID uint64, data, sig []byte) error {
	err := v.keys.Verify(keyID, data, sig)
	if err != nil && errors.Is(err, signing.ErrUnknownKey) && !v.required {
		v.unknown.Add(1)
		return nil
	}
	return err
}

// openSigning is how one open of a store signs and checks commits.
type openSigning struct {
	// actor signs the open's commits; zero for a read-only open, or a
	// writable one no SignWith covers.
	actor    ActorRef
	provider signer.Provider
	verifier *commitVerifier
	required bool
}

// prepareSigning works out how an open of dir signs and checks commits. A
// writable open signs with the actor SignWith set for it. Every open checks
// the log's signatures against the data root's keyring. A store whose marker
// requires signed commits refuses an open with no actor to sign, and a log
// holding a commit or a record no signature covers: graphene rejects an
// unsigned commit itself, but not a record written outside any batch.
func prepareSigning(dir, key string, readOnly bool) (openSigning, error) {
	var s openSigning
	marker, has, err := schema.ReadMarker(dir)
	if err != nil {
		return s, err
	}
	s.required = has && marker.Signing.Required
	if !readOnly {
		if resolve := signingFor(key); resolve != nil {
			p, err := resolve()
			if err != nil {
				return s, fmt.Errorf("graph: the actor that signs %s: %w", dir, err)
			}
			ref := p.Actor()
			s.provider = p
			s.actor = ActorRef{Name: ref.Name, ActorID: ref.ActorID, KeyID: ref.KeyID}
		} else if s.required {
			return s, fmt.Errorf("graph: %s requires signed commits, and this open has no actor to sign them", dir)
		}
	}
	// Read after the actor is resolved, which may have added its key.
	ring, err := signer.NewKeystore(filepath.Dir(dir)).Keyring()
	if err != nil {
		return s, fmt.Errorf("graph: %w", err)
	}
	s.verifier = &commitVerifier{keys: ring.Verifier(), required: s.required}
	if s.required {
		if err := checkAllSigned(dir); err != nil {
			return s, err
		}
	}
	return s, nil
}

// apply sets the signing side of graphene's options.
func (s *openSigning) apply(opts *graphenedisk.Options) {
	opts.Verifier = s.verifier
	opts.RequireSignedCommits = s.required
	if s.provider != nil {
		opts.Signer = s.provider.Signer()
		// A compaction attests its image as the open's actor.
		opts.AttestActorID = s.actor.ActorID
	}
}

// warnUnknown says, after an open, how many commits it could not check.
func (s *openSigning) warnUnknown(dir string) {
	n := s.verifier.unknown.Load()
	if n == 0 {
		return
	}
	msg := fmt.Sprintf("the graph store at %s holds %d commits signed by keys its keyring does not hold; they cannot be checked", dir, n)
	logging.GetLogger().Warn("graph: commits signed by unknown keys", zap.String("dir", dir), zap.Int64("commits", n))
	console.Errorln("warning: " + msg)
}

// checkAllSigned refuses a log with anything no signature covers.
func checkAllSigned(dir string) error {
	ls, err := inspectLog(dir)
	if err != nil {
		return err
	}
	if unsigned := ls.Commits - ls.Signed; unsigned > 0 || ls.Loose > 0 {
		return fmt.Errorf("graph: %s requires signed commits, and its log holds %d unsigned commits and %d records outside any commit",
			dir, unsigned, ls.Loose)
	}
	return nil
}

// RequireSigned marks h's store as requiring signed commits from now on,
// when its log holds nothing a signature does not cover and h signs its own
// commits. Every later open then refuses an unsigned commit, a record outside
// any commit, and a commit signed by a key the keyring does not hold.
func RequireSigned(h *Handle) error {
	if h == nil || h.readOnly {
		return errors.New("graph: requiring signed commits needs a writable handle")
	}
	if h.actor.Unattributed() {
		return fmt.Errorf("graph: %s is open with no actor to sign its commits", h.dir)
	}
	return h.ExclusiveWrite(func() error {
		if err := checkAllSigned(h.dir); err != nil {
			return err
		}
		marker, has, err := schema.ReadMarker(h.dir)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("graph: %s has no schema marker", h.dir)
		}
		marker.Signing.Required = true
		return schema.WriteMarker(h.dir, marker)
	})
}

// Actor is the actor h's commits are signed by; zero when they are not.
func (h *Handle) Actor() ActorRef {
	if h == nil {
		return ActorRef{}
	}
	return h.actor
}
