package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"brainiac/internal/catalog"
	"brainiac/internal/install"
)

// catalogKeyEnv names the file holding the owner's signing key, read only by
// `brainiac catalog sign`.
const catalogKeyEnv = "BRAINIAC_CATALOG_KEY"

// runCatalog handles `brainiac catalog pin|sign|verify|keygen`.
func runCatalog(args []string) error {
	usage := errors.New("usage: brainiac catalog pin <file> | sign <file> [--key f] | verify <file> | keygen --out <file>")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "pin":
		return runCatalogPin(args[1:])
	case "sign":
		return runCatalogSign(args[1:])
	case "verify":
		return runCatalogVerify(args[1:])
	case "keygen":
		return runCatalogKeygen(args[1:])
	}
	return usage
}

// catalogFile is a catalog file as `catalog pin` and `verify` read it: the
// whole catalog (internal/catalog/catalog.json) or an overlay
// (catalog.local.json), told apart by whether it lists roles.
type catalogFile struct {
	data    []byte
	models  []catalog.Model
	runtime *catalog.Runtime
}

func readCatalogFile(path string) (*catalogFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Roles json.RawMessage `json:"roles"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f := &catalogFile{data: data}
	if len(probe.Roles) > 0 {
		c, err := catalog.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		f.models, f.runtime = c.Models, &c.Runtime
		return f, nil
	}
	var o catalog.Overlay
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := catalog.Merge(o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.models, f.runtime = o.Models, o.Runtime
	return f, nil
}

// unpinned lists what a catalog file cannot install for want of a pin.
func (f *catalogFile) unpinned() []string {
	c := catalog.Catalog{Models: f.models}
	return c.Unpinned()
}

// runCatalogPin fills a catalog file's missing pins from the Hub's and
// GitHub's metadata (no model or archive is downloaded) and writes it back
// with nothing else changed, for a person to review and sign.
func runCatalogPin(args []string) error {
	fs := flag.NewFlagSet("catalog pin", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "say what would be pinned, write nothing")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: brainiac catalog pin <file> [--dry-run]")
	}
	path := fs.Arg(0)
	f, err := readCatalogFile(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := install.NewPinner()
	pinned, failed := p.Models(ctx, f.models)
	var missingAssets []string
	if f.runtime != nil {
		if missingAssets, err = p.Runtime(ctx, f.runtime); err != nil {
			failed["llama.cpp "+f.runtime.LlamaCppTag] = err
		}
	}
	out, err := catalog.WritePins(f.data, f.models, f.runtime)
	if err != nil {
		return err
	}
	fmt.Printf("Pinned %d sources", pinned)
	if f.runtime != nil {
		fmt.Printf(" and %d llama.cpp %s assets", len(f.runtime.LlamaCppSHA256), f.runtime.LlamaCppTag)
	}
	fmt.Println(".")
	if len(missingAssets) > 0 {
		fmt.Printf("llama.cpp assets GitHub gives no digest for (not installable): %s\n", strings.Join(missingAssets, ", "))
	}
	if len(failed) > 0 {
		keys := make([]string, 0, len(failed))
		for k := range failed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("Could not pin (these stay unpinned, and are refused for install):")
		for _, k := range keys {
			fmt.Printf("  %s: %v\n", k, failed[k])
		}
	}
	if *dry {
		return nil
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(catalog.SigPath(path)); err == nil {
		fmt.Printf("%s changed, so its signature no longer verifies: sign it again after review.\n", path)
	}
	fmt.Printf("Wrote %s. Review the diff, then `brainiac catalog sign %s`.\n", path, path)
	return nil
}

// runCatalogSign writes path.sig with the owner's key, read from --key or
// BRAINIAC_CATALOG_KEY. It refuses a file that does not parse.
func runCatalogSign(args []string) error {
	fs := flag.NewFlagSet("catalog sign", flag.ContinueOnError)
	keyPath := fs.String("key", os.Getenv(catalogKeyEnv), "the signing key file (default $"+catalogKeyEnv+")")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *keyPath == "" {
		return errors.New("usage: brainiac catalog sign <file> --key <keyfile> (or set " + catalogKeyEnv + ")")
	}
	path := fs.Arg(0)
	f, err := readCatalogFile(path)
	if err != nil {
		return err
	}
	priv, err := catalog.ReadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(catalog.SigPath(path), catalog.Sign(f.data, priv), 0o644); err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	fmt.Printf("Signed %s with key %s: %s\n", path, catalog.KeyID(pub), catalog.SigPath(path))
	if gaps := f.unpinned(); len(gaps) > 0 {
		fmt.Printf("Note: %d sources are unpinned and will be refused for install:\n  %s\n", len(gaps), strings.Join(gaps, "\n  "))
	}
	if _, err := catalog.Verify(f.data, catalog.Sign(f.data, priv)); err != nil {
		fmt.Printf("Note: this build does not trust key %s, so it would not use the file (put its public key in internal/catalog/sign.go)\n", catalog.KeyID(pub))
	}
	return nil
}

// runCatalogVerify checks path.sig against the keys this build trusts and
// lists what the file cannot install for want of a pin.
func runCatalogVerify(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: brainiac catalog verify <file>")
	}
	path := args[0]
	f, err := readCatalogFile(path)
	if err != nil {
		return err
	}
	id, err := catalog.VerifyFile(path)
	if err != nil {
		return err
	}
	fmt.Printf("%s: signed by key %s\n", path, id)
	if gaps := f.unpinned(); len(gaps) > 0 {
		fmt.Printf("%d sources are unpinned and will be refused for install:\n  %s\n", len(gaps), strings.Join(gaps, "\n  "))
	}
	return nil
}

// runCatalogKeygen makes the owner's signing key pair: the private seed to
// a new file only its owner can read, the public key to stdout for
// internal/catalog/sign.go. It never overwrites a file.
func runCatalogKeygen(args []string) error {
	fs := flag.NewFlagSet("catalog keygen", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the private key (a new file, kept off the repository)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("usage: brainiac catalog keygen --out <file>")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(fh, hex.EncodeToString(priv.Seed())); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	fmt.Printf("Private key written to %s (keep it off the repository and off the box).\n", *out)
	fmt.Printf("Public key %s:\n%s\n", catalog.KeyID(pub), hex.EncodeToString(pub))
	fmt.Println("Put the public key in releaseKeyHex (internal/catalog/sign.go), or build with -ldflags \"-X brainiac/internal/catalog.releaseKeyHex=<hex>\".")
	return nil
}
