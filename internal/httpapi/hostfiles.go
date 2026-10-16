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

// hostFileStatHandler says whether a file on the person's computer exists, its size and when it was last
// written - never its content. A desktop's worker cannot see the person's files, so this is how the check of a
// task that saved one in a host app ("save it as C:\...\report.xlsx in Excel") learns whether it is there. It
// answers only for a path written in that desktop's own open task request, so a desktop cannot probe the disk.
func (s *Server) hostFileStatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := desktop.SanitizeID(r.URL.Query().Get("desktop"))
	asked := strings.TrimSpace(r.URL.Query().Get("path"))
	if id == "" || !hostDrivePath.MatchString(asked) || s.Tasks == nil {
		http.Error(w, `desktop and a C:\ path are required`, http.StatusBadRequest)
		return
	}
	rel := strings.ReplaceAll(asked[3:], `\`, "/")
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			http.Error(w, "the path may not go up a folder", http.StatusBadRequest)
			return
		}
	}
	request, err := s.Tasks.OpenRequest(id)
	if err != nil {
		jsonError(w, err)
		return
	}
	if !namedIn(request, asked) {
		http.Error(w, "only a file named in this desktop's open task", http.StatusForbidden)
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
		info, err := os.Stat(filepath.Join(drive, filepath.FromSlash(path.Clean(rel))))
		if err != nil {
			continue
		}
		answer["exists"] = true
		answer["folder"] = info.IsDir()
		answer["size"] = info.Size()
		answer["modified"] = info.ModTime().UTC().Format(time.RFC3339)
		if !info.IsDir() {
			answer["kind"] = fileKind(filepath.Join(drive, filepath.FromSlash(path.Clean(rel))))
		}
		break
	}
	jsonResponse(w, answer)
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
