package update

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/evals"
	"brainiac/internal/install"
	"brainiac/internal/store"
)

// Files in the store that let a staged plan be served without a person at
// the machine (O-3.6): a release the owner signed that names the staged
// lock's bytes, and the local parity eval of that lock.
const (
	ReleaseFile = "release.json" // and release.json.sig beside it
	ParityFile  = "parity.json"
	// ReleaseKind marks a release manifest, so no other file the release
	// key signs (a catalog overlay) can be passed off as one.
	ReleaseKind = "brainiac.release/v1"
)

// Release is the owner's signed statement that a staged plan may be
// served: it names the sha256 of lock.next.json's exact bytes.
type Release struct {
	Kind    string    `json:"kind"`
	LockSHA string    `json:"lock_sha256"`
	Made    time.Time `json:"made"`
	Note    string    `json:"note,omitempty"`
}

// Parity is the local parity eval of a staged plan: checked cases of the
// request shapes this box serves, answered by the staged plan's models and
// by the served ones, in Results (by model and field, as evals.json keeps
// them). Fields are the fields the box serves.
type Parity struct {
	LockSHA string        `json:"lock_sha256"`
	At      time.Time     `json:"at"`
	Fields  []string      `json:"fields"`
	Results evals.Results `json:"results"`
}

// PromoteOptions say who is promoting.
type PromoteOptions struct {
	// ByHand is a person at this machine (`brainiac update promote` off an
	// appliance). Without it, and always on an appliance, only a signed
	// release that names the staged lock promotes, and only after the local
	// parity eval passed. A release file that is present is checked either
	// way: a release that does not verify is never ignored.
	ByHand bool
}

// ErrNotReleased is a promote that no person and no signed release asked for.
var ErrNotReleased = errors.New("no signed release names the staged plan")

// verifySignature checks a detached signature against the keys this build
// trusts (tests replace it).
var verifySignature = catalog.Verify

// MakeRelease is the release manifest for the lock file at lockPath and its
// detached signature, made with the release key.
func MakeRelease(lockPath string, priv ed25519.PrivateKey, note string, now time.Time) (manifest, sig []byte, err error) {
	sha, err := fileSHA(lockPath)
	if err != nil {
		return nil, nil, err
	}
	manifest, err = json.MarshalIndent(Release{Kind: ReleaseKind, LockSHA: sha, Made: now.UTC(), Note: note}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	manifest = append(manifest, '\n')
	return manifest, catalog.Sign(manifest, priv), nil
}

// readRelease reads the store's release manifest. present is false when
// there is none; an error is one that is there and cannot be trusted.
func readRelease(st *store.Store) (rel Release, keyID string, present bool, err error) {
	data, err := os.ReadFile(st.Path(ReleaseFile))
	if errors.Is(err, os.ErrNotExist) {
		return rel, "", false, nil
	}
	if err != nil {
		return rel, "", true, err
	}
	sig, err := os.ReadFile(catalog.SigPath(st.Path(ReleaseFile)))
	if err != nil {
		return rel, "", true, fmt.Errorf("%s has no signature (%s): %w", ReleaseFile, ReleaseFile+".sig", catalog.ErrBadSignature)
	}
	if keyID, err = verifySignature(data, sig); err != nil {
		return rel, "", true, fmt.Errorf("%s: %w", ReleaseFile, err)
	}
	if err := json.Unmarshal(data, &rel); err != nil {
		return rel, "", true, fmt.Errorf("%s: %w", ReleaseFile, err)
	}
	if rel.Kind != ReleaseKind {
		return rel, "", true, fmt.Errorf("%s is signed but is not a release manifest (kind %q)", ReleaseFile, rel.Kind)
	}
	return rel, keyID, true, nil
}

// releaseGate decides whether the staged plan may be promoted: by a person
// (opt.ByHand, no release present), or by a signed release naming s's lock
// whose local parity eval passed. It says why not.
func releaseGate(st *store.Store, s Staged, cur, next *install.Lock, opt PromoteOptions) error {
	rel, _, present, err := readRelease(st)
	switch {
	case err != nil:
		return fmt.Errorf("not promoted: %w", err)
	case !present && opt.ByHand:
		return nil
	case !present:
		return fmt.Errorf("not promoted: %w (%s signed with the release key; on an appliance a person cannot promote by hand)", ErrNotReleased, ReleaseFile)
	case !strings.EqualFold(rel.LockSHA, s.LockSHA):
		return fmt.Errorf("not promoted: the signed release names lock %s, but the staged %s is %s", short(rel.LockSHA), NextLock, short(s.LockSHA))
	}
	var p Parity
	if err := st.ReadJSON(ParityFile, &p); err != nil {
		return fmt.Errorf("not promoted: the release is signed, but no local parity eval of the staged plan was found (%s)", ParityFile)
	}
	return p.check(s, cur, next)
}

// check says whether the parity eval shows the staged plan is not worse
// than the served one where this box works: for every changed role, the new
// model is proven (evals.Results.Proven: the Wilson lower bound of its
// decided cases at least the old model's accuracy, 20 cases or more) at
// least as good as the model it replaces, in every field the box serves for
// the brain, and in the role's own field for any other role. Every score
// must be from after the plan was staged and checked, on these lock bytes.
func (p Parity) check(s Staged, cur, next *install.Lock) error {
	switch {
	case !strings.EqualFold(p.LockSHA, s.LockSHA):
		return fmt.Errorf("not promoted: the parity eval measured lock %s, not the staged %s", short(p.LockSHA), short(s.LockSHA))
	case len(p.Fields) == 0:
		return errors.New("not promoted: the parity eval names no field this box serves")
	}
	var gaps []string
	for _, ch := range s.Changes {
		from, to := ch.From, ch.To
		if from == "" {
			from = brainOfLock(cur) // the brain answered the role before
		}
		if to == "" {
			to = brainOfLock(next)
		}
		if from == "" || to == "" || from == to {
			continue
		}
		fields := []string{ch.Role}
		if ch.Role == catalog.RoleBrain {
			fields = p.Fields
		}
		for _, f := range fields {
			if why := p.provenWhy(to, from, f, s.At); why != "" {
				gaps = append(gaps, fmt.Sprintf("%s %s: %s", ch.Role, f, why))
			}
		}
	}
	if len(gaps) > 0 {
		sort.Strings(gaps)
		return fmt.Errorf("not promoted: the local parity eval does not show the staged plan is as good as the served one: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// provenWhy is "" when to is proven at least as good as from in field f on
// scores made after the plan was staged, else the reason it is not.
func (p Parity) provenWhy(to, from, f string, staged time.Time) string {
	st, okT := p.Results.Scores[to][f]
	sf, okF := p.Results.Scores[from][f]
	switch {
	case !okT || !okF:
		return fmt.Sprintf("%s and %s are not both measured", to, from)
	case st.At.Before(staged) || sf.At.Before(staged):
		return "measured before the plan was staged"
	case !p.Results.Proven(to, from, f):
		return fmt.Sprintf("%s (%d/%d decided correct) is not proven as good as %s (%d/%d)", to, st.DecidedCorrect, st.Decided, from, sf.DecidedCorrect, sf.Decided)
	}
	return ""
}

func brainOfLock(l *install.Lock) string {
	if l == nil || l.Plan == nil {
		return ""
	}
	if b, ok := l.Plan.Pick(catalog.RoleBrain); ok {
		return b.Model
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// WriteParity records a parity eval of the staged plan, for Promote.
func WriteParity(st *store.Store, p Parity) error { return st.WriteJSON(ParityFile, p) }
