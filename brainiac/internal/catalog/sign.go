package catalog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// releaseKeyHex is the public half of the owner's catalog signing key, as
// hex. It is compiled into the binary: either written here once the owner
// has made the key (`brainiac catalog keygen`), or set at build time with
// -ldflags "-X brainiac/internal/catalog.releaseKeyHex=<hex>". The private
// half never enters the repository; `brainiac catalog sign` reads it from a
// file at signing time. Empty means this build trusts no key: no overlay
// verifies, so an appliance runs the shipped catalog alone.
var releaseKeyHex = ""

// extraKeys are keys tests add (a key pair made for the test alone).
var extraKeys []ed25519.PublicKey

// ErrBadSignature is a catalog file whose signature is missing, malformed,
// or not made by a key this build trusts.
var ErrBadSignature = errors.New("the catalog's signature does not verify")

// SigPath is where a catalog file's detached signature lives.
func SigPath(path string) string { return path + ".sig" }

// trusted are the keys this build accepts.
func trusted() []ed25519.PublicKey {
	keys := append([]ed25519.PublicKey(nil), extraKeys...)
	for _, h := range strings.Fields(strings.ReplaceAll(releaseKeyHex, ",", " ")) {
		if b, err := hex.DecodeString(h); err == nil && len(b) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(b))
		}
	}
	return keys
}

// HasReleaseKey reports whether this build trusts any signing key.
func HasReleaseKey() bool { return len(trusted()) > 0 }

// KeyID is a short name for a public key: the first 8 bytes of its sha256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign is the detached signature of a catalog file's exact bytes, as the
// .sig file holds it (base64 and a newline).
func Sign(data []byte, priv ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n")
}

// Verify checks a detached signature (the .sig file's content) over a
// catalog file's exact bytes against the keys this build trusts, and
// returns the id of the key that made it.
func Verify(data, sig []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: the signature is not a base64 ed25519 signature", ErrBadSignature)
	}
	keys := trusted()
	if len(keys) == 0 {
		return "", fmt.Errorf("%w: this build trusts no signing key", ErrBadSignature)
	}
	for _, k := range keys {
		if ed25519.Verify(k, data, raw) {
			return KeyID(k), nil
		}
	}
	return "", fmt.Errorf("%w: no key this build trusts made it, or the file changed after signing", ErrBadSignature)
}

// VerifyFile checks path against path.sig.
func VerifyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sig, err := os.ReadFile(SigPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s has no signature (%s)", ErrBadSignature, path, SigPath(path))
	}
	if err != nil {
		return "", err
	}
	return Verify(data, sig)
}

// ReadPrivateKey reads a signing key file: the 32-byte seed (or the 64-byte
// private key) as hex, as `brainiac catalog keygen` writes it.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(data)))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: not a hex key", path)
	case len(b) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("%s: a key is %d or %d bytes, not %d", path, ed25519.SeedSize, ed25519.PrivateKeySize, len(b))
}

// ReadTrustedOverlay reads the overlay at path and decides whether it may
// be used. An overlay with a signature beside it is used only when the
// signature verifies, wherever Brainiac runs: a signed file that no longer
// verifies was changed by someone other than its signer. Without a
// signature it is the operator's own (`scout adopt`, the updater) and is
// used, unless signedOnly (an appliance), where only a signed overlay is.
// An overlay that is not used is returned empty, with a note saying why.
func ReadTrustedOverlay(path string, signedOnly bool) (Overlay, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Overlay{}, "", nil
	}
	if err != nil {
		return Overlay{}, "", err
	}
	if _, serr := os.Stat(SigPath(path)); serr == nil || signedOnly {
		sig, err := os.ReadFile(SigPath(path))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Overlay{}, fmt.Sprintf("%s is not signed; on an appliance only a signed catalog overlay is used, so the shipped catalog runs alone", path), nil
			}
			return Overlay{}, "", err
		}
		if _, err := Verify(data, sig); err != nil {
			return Overlay{}, fmt.Sprintf("%s is not used: %v", path, err), nil
		}
	}
	o, err := parseOverlay(data, path) // the bytes that were verified, not a second read
	return o, "", err
}
