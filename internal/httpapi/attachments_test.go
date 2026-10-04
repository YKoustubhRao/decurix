package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// uploadRequest is a multipart POST carrying fields and files (name, data).
func uploadRequest(t *testing.T, path string, fields map[string]string, files [][2]string) *http.Request {
	t.Helper()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}

	for _, f := range files {
		part, err := w.CreateFormFile(attachmentField, f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())

	return req
}

func TestARequestNamesTheFilesItBroughtAndIsTakenApartAgain(t *testing.T) {
	paths := []string{"/home/headless/work/inbox/up-1/PO.pdf", `C:\Users\ana\workers\w1\home\work\inbox\up-1\scan.png`}

	sent := withAttachments("Enter this purchase order\n", paths)
	want := "Enter this purchase order\n\n" + attachmentsMark + "\n  " + paths[0] + "\n  " + paths[1]
	if sent != want {
		t.Fatalf("request = %q, want %q", sent, want)
	}

	text, files := splitAttachments(sent)
	if text != "Enter this purchase order" || !reflect.DeepEqual(files, paths) {
		t.Errorf("split = %q, %q", text, files)
	}

	if got := withAttachments("hello", nil); got != "hello" {
		t.Errorf("a request with no files was changed: %q", got)
	}
	if text, files := splitAttachments("hello"); text != "hello" || files != nil {
		t.Errorf("a request with no files split into %q, %q", text, files)
	}
}

// The desktop's agent takes a request apart on the same words.
func TestTheAgentSplitsARequestOnTheSameMark(t *testing.T) {
	source, err := os.ReadFile("../../agent/decurix_agent/attachments.py")
	if err != nil {
		t.Skip("the agent's source is not here:", err)
	}

	if !strings.Contains(string(source), `MARK = "`+attachmentsMark+`"`) {
		t.Errorf("agent/decurix_agent/attachments.py's MARK is not %q", attachmentsMark)
	}
}

func TestAnAttachmentsNameIsOneAPathInARequestCanHold(t *testing.T) {
	for name, want := range map[string]string{
		"PO 7731 (final).pdf":     "PO_7731_final.pdf",
		`C:\Users\me\invoice.pdf`: "invoice.pdf",
		"../../etc/passwd":        "passwd",
		"scan.PNG":                "scan.PNG",
		"résumé.docx":             "r_sum.docx",
		"請求書.pdf":                 "file.pdf",
		"notes":                   "notes",
		"   ":                     "file",
		"..":                      "file",
	} {
		if got := attachmentName(name); got != want {
			t.Errorf("attachmentName(%q) = %q, want %q", name, got, want)
		}
	}

	seen := map[string]int{}
	got := []string{uniqueAttachmentName("scan.pdf", seen), uniqueAttachmentName("Scan.pdf", seen), uniqueAttachmentName("scan.pdf", seen)}
	if want := []string{"scan.pdf", "Scan_2.pdf", "scan_3.pdf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestFilesAreReadFromAnUploadAndRefusedWhenTheyCannotBeSent(t *testing.T) {
	req := uploadRequest(t, "/api/desktops/send", map[string]string{"id": "ada", "content": "enter it"},
		[][2]string{{"PO 1.pdf", "%PDF-1"}, {"PO 1.pdf", "%PDF-2"}})

	files, err := readAttachments(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "PO_1.pdf" || files[1].Name != "PO_1_2.pdf" || string(files[1].Data) != "%PDF-2" {
		t.Errorf("files = %+v", files)
	}
	if req.FormValue("content") != "enter it" {
		t.Errorf("the request's words were lost: %q", req.FormValue("content"))
	}

	// An ordinary form-encoded request carries none, and is left alone.
	plain := httptest.NewRequest(http.MethodPost, "/api/desktops/send", strings.NewReader("id=ada&content=hi"))
	plain.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if files, err := readAttachments(plain); err != nil || files != nil {
		t.Errorf("a form-encoded request: %v, %v", files, err)
	}

	var many [][2]string
	for i := 0; i <= maxAttachments; i++ {
		many = append(many, [2]string{"a.txt", "x"})
	}
	for name, req := range map[string]*http.Request{
		"too many":   uploadRequest(t, "/x", nil, many),
		"empty file": uploadRequest(t, "/x", nil, [][2]string{{"empty.pdf", ""}}),
	} {
		var mistake *errUpload
		if _, err := readAttachments(req); err == nil || !asUpload(err, &mistake) {
			t.Errorf("%s: err = %v, want a refusal the person can act on", name, err)
		}
	}
}

func asUpload(err error, target **errUpload) bool {
	e, ok := err.(*errUpload)
	*target = e

	return ok
}

// Files alone are a request; nothing at all is not.
func TestSendingNothingIsRefused(t *testing.T) {
	s := &Server{}

	rec := httptest.NewRecorder()
	s.sendRequestHandler(rec, uploadRequest(t, "/api/desktops/send", map[string]string{"id": "ada"}, nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("a request with no words and no files answered %d", rec.Code)
	}
}
