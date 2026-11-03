package httpapi

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// hostDrivePath is an absolute path on the host's C: drive, as a person writes one in a request.
var hostDrivePath = regexp.MustCompile(`(?i)^c:[\\/]`)

// hostCDrives is where the host's C: drive is read; a variable so tests can point it at a folder.
var hostCDrives = desktop.WindowsCDrives

// hostFileAsked checks a request for a host file: a desktop, a C:\ path that does not go up a folder, and written in
// that desktop's own open task request - so a desktop can ask only about what its task named, never probe the disk.
// It answers the path asked for and its place under the C: drive, or writes the refusal and answers ok=false. Any
// method but method is refused.
func (s *Server) hostFileAsked(w http.ResponseWriter, r *http.Request, method string) (asked, rel string, ok bool) {
	if !allowMethod(w, r, method) {
		return "", "", false
	}
	claimed, ok := actingAs(w, r, r.URL.Query().Get("desktop"))
	if !ok {
		return "", "", false
	}
	id := desktop.SanitizeID(claimed)
	asked = strings.TrimSpace(r.URL.Query().Get("path"))
	if strings.EqualFold(path.Base(strings.ReplaceAll(asked, "\\", "/")), QualificationOwnerFile) {
		http.Error(w, "readiness owner keys cannot be read by a desktop", http.StatusForbidden)
		return "", "", false
	}
	if id == "" || !hostDrivePath.MatchString(asked) || s.Tasks == nil {
		http.Error(w, `desktop and a C:\ path are required`, http.StatusBadRequest)
		return "", "", false
	}
	rel = strings.ReplaceAll(asked[3:], `\`, "/")
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			http.Error(w, "the path may not go up a folder", http.StatusBadRequest)
			return "", "", false
		}
		if part == "" {
			continue
		}
		// Decided on as written here, and again as it resolves on the disk (resolveHostFile).
		if !plainHostName(part) || privateHostFolder(part) {
			http.Error(w, refusedHostFile, http.StatusForbidden)
			return "", "", false
		}
	}
	request, err := s.Tasks.OpenRequest(id)
	if err != nil {
		jsonError(w, err)
		return "", "", false
	}
	if !namedIn(request, asked) {
		http.Error(w, "only a file named in this desktop's open task", http.StatusForbidden)
		return "", "", false
	}
	return asked, path.Clean(rel), true
}

// hostFileStatHandler says whether a file on the person's computer exists, its size, when it was last written and
// what its first bytes make it. A desktop's worker cannot see the person's files, so this is how the check of a task
// that saved one in a host app ("save it as C:\...eport.xlsx in Excel") learns whether it is there.
func (s *Server) hostFileStatHandler(w http.ResponseWriter, r *http.Request) {
	asked, rel, ok := s.hostFileAsked(w, r, http.MethodGet)
	if !ok {
		return
	}
	answer := map[string]any{"path": asked, "exists": false}
	drives := hostCDrives()
	if len(drives) == 0 {
		answer["unknown"] = true
		jsonResponse(w, answer)
		return
	}
	for _, drive := range drives {
		full, err := resolveHostFile(drive, rel)
		if errors.Is(err, errHostFileRefused) {
			http.Error(w, refusedHostFile, http.StatusForbidden)
			return
		}
		if err != nil {
			continue
		}
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		answer["exists"] = true
		answer["folder"] = info.IsDir()
		answer["size"] = info.Size()
		answer["modified"] = info.ModTime().UTC().Format(time.RFC3339)
		if !info.IsDir() {
			answer["kind"] = fileKind(full)
		}
		break
	}
	jsonResponse(w, answer)
}

// hostFileReadMax is the largest file of the person's computer handed to a check.
const hostFileReadMax = 30 << 20

// hostFileReadHandler hands a desktop's final check the content of the one file its open task named on the
// person's computer, so the check can read what was saved (a workbook's cells, a letter's words) and not only that a
// file of that name exists: found live (2026-10-16), correct-looking files with a header that was not bold and a total
// over damaged data were reported done. The same rule as the stat: only a path the task's own request names.
func (s *Server) hostFileReadHandler(w http.ResponseWriter, r *http.Request) {
	_, rel, ok := s.hostFileAsked(w, r, http.MethodGet)
	if !ok {
		return
	}
	if !readableHostFile(rel) {
		http.Error(w, "only a document a task saves (a workbook, a letter, a PDF, a CSV, text, data, a picture) is read for a desktop", http.StatusForbidden)
		return
	}
	for _, drive := range hostCDrives() {
		full, err := resolveHostFile(drive, rel)
		if errors.Is(err, errHostFileRefused) {
			http.Error(w, refusedHostFile, http.StatusForbidden)
			return
		}
		if err != nil {
			continue
		}
		if strings.EqualFold(filepath.Base(full), QualificationOwnerFile) || !readableHostFile(filepath.ToSlash(full)) {
			http.Error(w, refusedHostFile, http.StatusForbidden)
			return
		}
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			continue
		}
		if info.Size() > hostFileReadMax {
			f.Close()
			http.Error(w, "the file is too large to read", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), f)
		f.Close()
		return
	}
	http.Error(w, "no such file", http.StatusNotFound)
}

// The host-file checks read the person's own disk for a desktop, on a path its task names, and a task's text can come
// from an email. Deciding on the path as written was beaten (review 2026-11-02): the "Application Data" and "Local
// Settings" junctions lead into AppData, Windows drops a trailing dot or space ("AppData."), an 8.3 short name
// ("APPDAT~1") is the same folder, and a symlink or junction can point anywhere. So the path is decided on as it
// resolves: each part must be a name the folder above actually lists (which an 8.3 alias, a trailing dot and a
// different case are not - the listed name is used), every link is followed and the place it leads is checked again
// from the drive's root, and a path that cannot be resolved that way is refused.

// refusedHostFile is the answer to a host path a desktop may not have read.
const refusedHostFile = "files under AppData, ProgramData, Windows, a hidden folder, or behind a link to one, are not read for a desktop"

// errHostFileRefused is a host path that leads somewhere a desktop may not have read, or cannot be resolved.
var errHostFileRefused = errors.New("refused host path")

// privateHostFolder reports whether a folder or file name is one whose content belongs to programs, not to a task: a
// program's own data (browser profiles with saved passwords and cookies, keys, tokens) lives under AppData (and its
// old names, the junctions "Application Data" and "Local Settings"), ProgramData or a dot-folder, and Windows' own
// files under Windows.
func privateHostFolder(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	for _, private := range []string{"AppData", "Application Data", "Local Settings", "ProgramData", "Windows", "NTUSER.DAT"} {
		if strings.EqualFold(name, private) {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(name), "ntuser.")
}

// plainHostName reports whether a path part is a name Windows reads as written: not empty, no trailing dot or space
// (Windows drops them, so "AppData." is AppData), no 8.3 short-name tail ("APPDAT~1"), no stream (":"), no device name.
func plainHostName(name string) bool {
	if name == "" || strings.TrimRight(name, ". ") != name || strings.ContainsAny(name, ":*?\"<>|") {
		return false
	}
	if shortNameTail.MatchString(name) {
		return false
	}
	stem := strings.ToUpper(strings.TrimSpace(strings.SplitN(name, ".", 2)[0]))
	return !reservedDeviceName.MatchString(stem)
}

// shortNameTail is an 8.3 short name's "~N" before the extension.
var shortNameTail = regexp.MustCompile(`~[0-9]+(\.[^.]*)?$`)

// reservedDeviceName is a name Windows opens as a device, whatever folder it is in.
var reservedDeviceName = regexp.MustCompile(`^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[0-9¹²³]|LPT[0-9¹²³])$`)

// readableHostDocument is the kind of file a task saves in a host app, by extension: what a check may read the content
// of. A key, a database, a browser profile's files and a config have none of these.
var readableHostDocument = map[string]bool{
	".xlsx": true, ".xlsm": true, ".xlsb": true, ".xls": true, ".ods": true, ".csv": true, ".tsv": true,
	".docx": true, ".docm": true, ".doc": true, ".odt": true, ".rtf": true, ".txt": true, ".md": true,
	".pptx": true, ".ppt": true, ".odp": true, ".pdf": true, ".json": true, ".xml": true, ".html": true, ".htm": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".tif": true, ".tiff": true, ".zip": true,
}

// readableHostFile reports whether the file p names is a document a check may read the content of.
func readableHostFile(p string) bool {
	return readableHostDocument[strings.ToLower(path.Ext(p))]
}

// maxHostLinks bounds the links followed while resolving one host path.
const maxHostLinks = 8

// resolveHostFile is rel (slash-separated, under the drive) on drive, resolved: each part as its folder lists it,
// every link followed and the place it leads checked again from the root. It answers errHostFileRefused for a path
// that leads under a private folder (privateHostFolder), out of the drive, or that cannot be resolved, and
// fs.ErrNotExist for one that is simply not there.
func resolveHostFile(drive, rel string) (string, error) {
	root, err := filepath.EvalSymlinks(drive)
	if err != nil {
		return "", errHostFileRefused
	}
	root = filepath.Clean(root)
	pending := splitHostPath(rel)
	current := root
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "." {
			continue
		}
		if part == ".." || !plainHostName(part) {
			return "", errHostFileRefused
		}
		listed, err := listedName(current, part)
		if err != nil {
			return "", err
		}
		if privateHostFolder(listed) {
			return "", errHostFileRefused
		}
		next := filepath.Join(current, listed)
		info, err := os.Lstat(next)
		if err != nil {
			return "", errHostFileRefused
		}
		if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
			current = next
			continue
		}
		// A link (a symlink, or a junction - which Go reports as irregular on Windows and EvalSymlinks no longer
		// follows): where it leads is decided on again from the drive's root, part by part. current has no link in
		// it, so a relative target cleaned against it is where the link leads.
		if links++; links > maxHostLinks {
			return "", errHostFileRefused
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", errHostFileRefused
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(current, target)
		}
		// In the same spelling as root (long names on Windows, no links above it); the walk below checks every part
		// of what this gives once more.
		target = filepath.Clean(target)
		parent, err := filepath.EvalSymlinks(filepath.Dir(target))
		if err != nil {
			return "", errHostFileRefused
		}
		inside, err := filepath.Rel(root, filepath.Join(parent, filepath.Base(target)))
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
			return "", errHostFileRefused
		}
		pending = append(splitHostPath(filepath.ToSlash(inside)), pending...)
		current = root
	}
	return current, nil
}

// splitHostPath is a slash-separated path's parts, empty ones dropped.
func splitHostPath(p string) []string {
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// listedName is the name in folder dir that name means: the entry dir lists under exactly that name, or the one entry
// that differs from it only in case (Windows folders ignore case). An 8.3 alias or a name Windows would trim is no
// listed entry. It answers fs.ErrNotExist when dir lists no such entry, and errHostFileRefused when it cannot be read
// or two entries match.
func listedName(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fs.ErrNotExist
		}
		return "", errHostFileRefused
	}
	found := ""
	for _, e := range entries {
		if e.Name() == name {
			return name, nil
		}
		if strings.EqualFold(e.Name(), name) {
			if found != "" {
				return "", errHostFileRefused
			}
			found = e.Name()
		}
	}
	if found == "" {
		return "", fs.ErrNotExist
	}
	return found, nil
}

// namedIn reports whether a request mentions a host path: the whole path, or its folder and its file name
// apart ("in C:\Users\me\Documents ... save it as report.xlsx"). Slashes and case do not matter.
func namedIn(request, asked string) bool {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "/", `\`)) }
	text, want := norm(request), strings.TrimRight(norm(asked), `\`)
	if want == "" {
		return false
	}
	if strings.Contains(text, want) {
		return true
	}
	cut := strings.LastIndex(want, `\`)
	if cut < 3 {
		return false
	}
	folder, name := want[:cut], want[cut+1:]
	return name != "" && strings.Contains(text, folder) && strings.Contains(text, name)
}

// fileKind says what a file's first bytes make it - "zip" (every .xlsx, .docx and .pptx), "pdf", "ole" (an old .xls,
// .doc or .ppt), "text" or "other" - so a CSV saved under a workbook's name is not taken for the workbook. Eight bytes
// are read, nothing more.
func fileKind(name string) string {
	f, err := os.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := f.Read(head)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "zip"
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "pdf"
	case bytes.HasPrefix(head, []byte{0xD0, 0xCF, 0x11, 0xE0}):
		return "ole"
	case n > 0 && utf8.Valid(head) && !bytes.ContainsRune(head, 0):
		return "text"
	}
	return "other"
}
