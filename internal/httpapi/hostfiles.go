package httpapi

import (
	"bytes"
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
// It answers the path asked for and its place under the C: drive, or writes the refusal and answers ok=false.
func (s *Server) hostFileAsked(w http.ResponseWriter, r *http.Request) (asked, rel string, ok bool) {
	return s.hostFileAskedBy(w, r, http.MethodGet)
}

// hostFileAskedBy is hostFileAsked for a request made with method.
func (s *Server) hostFileAskedBy(w http.ResponseWriter, r *http.Request, method string) (asked, rel string, ok bool) {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return "", "", false
	}
	id := desktop.SanitizeID(r.URL.Query().Get("desktop"))
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
	asked, rel, ok := s.hostFileAsked(w, r)
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
		full := filepath.Join(drive, filepath.FromSlash(rel))
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
	_, rel, ok := s.hostFileAsked(w, r)
	if !ok {
		return
	}
	for _, drive := range hostCDrives() {
		full := filepath.Join(drive, filepath.FromSlash(rel))
		if resolved, err := filepath.EvalSymlinks(full); err == nil && strings.EqualFold(filepath.Base(resolved), QualificationOwnerFile) {
			http.Error(w, "readiness owner keys cannot be read by a desktop", http.StatusForbidden)
			return
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > hostFileReadMax {
			http.Error(w, "the file is too large to read", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, full)
		return
	}
	http.Error(w, "no such file", http.StatusNotFound)
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
