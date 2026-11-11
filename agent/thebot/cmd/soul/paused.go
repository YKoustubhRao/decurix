// paused.go: each session's paused job kept on disk, so a person's reply still finds it after soul or its container
// restarts (an idle-stop, a recreate of the process, a host reboot). One file per session under
// <WorkspaceRoot>/.memory/paused (outside what the Data RAG ingests), mode 0600, written whole (a temporary file
// renamed over it), removed when the job is taken, replaced or expires, and read back at startup within pausedJobTTL.
//
// What a file never holds: a job's secret values. A checkpoint that had any (Checkpoint.Secrets, C14), or whose text
// holds a value soul was told is secret or looks like a secret by its shape (memory.RedactSecrets, the filter traces
// go through), is written as a stub with HadSecrets and nothing of the job; after a restart such a stub is dropped,
// and the reply finds nothing waiting, as it did before paused jobs were kept.
//
// The file is in the worker's home, which the worker's own commands can write. Each file is signed (HMAC-SHA256)
// with a key derived from the desktop's agent token, which no program of the worker's can read (agenttoken.go), and
// names its session inside what is signed: a file edited, made up or moved to another session is dropped. Without an
// agent token nothing is kept. A restored approval is resumed only through the gate's settlement of its own request,
// as before (gate.go): a gate that no longer knows the request settles nothing, and the reply approves nothing.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"thebot/internal/memory"
	"thebot/internal/scheduler"
	"thebot/internal/withheld"
)

// pausedHeader starts every paused file, before the body's signature.
const pausedHeader = "decurix-soul-paused/1"

// maxPausedFile bounds what is read of one paused file (a goal's transcript with its screenshots).
const maxPausedFile = 256 << 20

// pausedFile is the signed body of a paused file.
type pausedFile struct {
	Session   string    `json:"session"`
	CreatedAt time.Time `json:"createdAt"`
	// HadSecrets marks a stub: the job had secret values (or text that looks like one), so nothing of it was written
	// and it is never resumed after a restart.
	HadSecrets bool `json:"hadSecrets,omitempty"`
	// Checkpoint is scheduler.EncodeCheckpoint's data; empty in a stub.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
}

// pausedStore is where paused jobs are kept on disk; nil keeps none.
type pausedStore struct {
	dir string
	key []byte
}

// newPausedStore is the store under root, with its key derived from agentToken; nil without a token.
func newPausedStore(root, agentToken string) *pausedStore {
	if root == "" || agentToken == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(agentToken))
	mac.Write([]byte("decurix-soul-paused\x00"))
	return &pausedStore{dir: filepath.Join(root, ".memory", "paused"), key: mac.Sum(nil)}
}

// path is the file of session's paused job: named by a hash, so no session id is a path.
func (p *pausedStore) path(session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(p.dir, hex.EncodeToString(sum[:16])+".json")
}

// sign is the signature of body.
func (p *pausedStore) sign(body []byte) []byte {
	mac := hmac.New(sha256.New, p.key)
	mac.Write([]byte(pausedHeader + "\x00"))
	mac.Write(body)
	return mac.Sum(nil)
}

// save writes session's paused job, replacing what the session had. known is every secret value soul was told this
// process. A checkpoint that cannot be encoded is kept in memory only, and the session's old file is removed.
func (p *pausedStore) save(session string, cp *scheduler.Checkpoint, known withheld.Values) {
	if p == nil || session == "" || cp == nil {
		return
	}
	body := pausedFile{Session: session, CreatedAt: cp.CreatedAt}
	if len(cp.Secrets) > 0 {
		body.HadSecrets = true
	} else {
		raw, err := scheduler.EncodeCheckpoint(cp)
		if err != nil {
			log.Printf("[soul] paused: the paused job cannot be kept on disk (%v) - kept in memory only", err)
			p.remove(session)
			return
		}
		if looksSecret(raw, known) {
			body.HadSecrets = true
		} else {
			body.Checkpoint = raw
		}
	}
	if body.HadSecrets {
		log.Printf("[soul] paused: the paused job holds secret values - only a marker is kept on disk; after a restart " +
			"it is not resumed")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		log.Printf("[soul] paused: %v - kept in memory only", err)
		p.remove(session)
		return
	}
	var out bytes.Buffer
	out.WriteString(pausedHeader + " " + hex.EncodeToString(p.sign(encoded)) + "\n")
	out.Write(encoded)
	if err := p.write(p.path(session), out.Bytes()); err != nil {
		log.Printf("[soul] paused: could not keep the paused job on disk (%v) - kept in memory only", err)
		p.remove(session)
	}
}

// looksSecret reports whether any text of an encoded checkpoint holds one of known, or is changed by soul's secrets
// filter (memory.RedactSecrets). Pictures are not text and are not read.
func looksSecret(raw []byte, known withheld.Values) bool {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return true
	}
	found := false
	var walk func(x any)
	walk = func(x any) {
		if found {
			return
		}
		switch t := x.(type) {
		case string:
			if known.Holds(t) || memory.RedactSecrets(t) != t {
				found = true
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		case map[string]any:
			for k, v := range t {
				if k == "Images" || k == "lookShot" { // pictures, as base64: not text
					continue
				}
				walk(k)
				walk(v)
			}
		}
	}
	walk(decoded)
	return found
}

// write puts b at path whole: a temporary file in the folder (0600), synced, then renamed over it.
func (p *pausedStore) write(path string, b []byte) error {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(p.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Chmod(0o600) // CreateTemp makes it 0600 already; said again for what reads this
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// remove deletes session's paused file, if there is one.
func (p *pausedStore) remove(session string) {
	if p == nil || session == "" {
		return
	}
	if err := os.Remove(p.path(session)); err != nil && !os.IsNotExist(err) {
		log.Printf("[soul] paused: could not remove a paused job's file: %v", err)
	}
}

// load reads every paused job kept on disk that may still be resumed: signed by this desktop, for the session its
// file is named after, within ttl, and with no secret values. Every other file is removed, with the reason logged.
func (p *pausedStore) load(ttl time.Duration) map[string]*scheduler.Checkpoint {
	out := map[string]*scheduler.Checkpoint{}
	if p == nil {
		return out
	}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[soul] paused: could not read %s: %v", p.dir, err)
		}
		return out
	}
	for _, e := range entries {
		path := filepath.Join(p.dir, e.Name())
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			_ = os.Remove(path) // a write a restart cut short
			continue
		}
		session, cp, why := p.read(path, ttl)
		if cp == nil {
			log.Printf("[soul] paused: dropped a paused job kept on disk (%s): its reply will find nothing waiting", why)
			_ = os.Remove(path)
			continue
		}
		out[session] = cp
	}
	if len(out) > 0 {
		log.Printf("[soul] paused: %d paused job(s) kept across the restart", len(out))
	}
	return out
}

// read is the session and checkpoint in path, or nil and why it may not be resumed.
func (p *pausedStore) read(path string, ttl time.Duration) (string, *scheduler.Checkpoint, string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, "unreadable"
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPausedFile+1))
	f.Close()
	if err != nil || len(raw) > maxPausedFile {
		return "", nil, "unreadable or too large"
	}
	header, body, ok := bytes.Cut(raw, []byte("\n"))
	if !ok {
		return "", nil, "not a paused file"
	}
	fields := strings.Fields(string(header))
	if len(fields) != 2 || fields[0] != pausedHeader {
		return "", nil, "not a paused file of this version"
	}
	sig, err := hex.DecodeString(fields[1])
	if err != nil || !hmac.Equal(sig, p.sign(body)) {
		return "", nil, "its signature does not match: it was changed, or not written by this desktop's soul"
	}
	var pf pausedFile
	if err := json.Unmarshal(body, &pf); err != nil {
		return "", nil, "unreadable body"
	}
	if pf.Session == "" || p.path(pf.Session) != path {
		return "", nil, "it is not the file of the session it names"
	}
	if time.Since(pf.CreatedAt) > ttl {
		return "", nil, "it waited longer than a paused job may"
	}
	if pf.HadSecrets {
		return "", nil, "it held secret values, which are never written, so it cannot be resumed"
	}
	cp, err := scheduler.DecodeCheckpoint(pf.Checkpoint)
	if err != nil {
		return "", nil, fmt.Sprintf("its checkpoint does not decode: %v", err)
	}
	if !cp.CreatedAt.Equal(pf.CreatedAt) {
		return "", nil, "its checkpoint is not the one it names"
	}
	return pf.Session, cp, ""
}
