package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// office read prints one header per document and its page pictures below it; each picture is labelled with its own
// document, whatever order they come in.
func TestEachScannedPageIsLabelledWithItsOwnDocument(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, d := range []string{"a", "b"} {
		p := filepath.Join(dir, d, "page-1.png")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(pngHead+"png "+d), 0o644)
		paths = append(paths, p)
	}
	output := "scan_0030.pdf: PDF, 1 page(s); pages 1-1. Pages 1-1 are shown as pictures below.\n[page image: " + paths[0] +
		"]\nscan_0031.pdf: PDF, 1 page(s); pages 1-1. Pages 1-1 are shown as pictures below.\n[page image: " + paths[1] + "]\n"
	st := &operatorState{Vision: true}
	pics, labels := st.documentPictures(output)
	if len(pics) != 2 || string(pics[1]) != pngHead+"png b" {
		t.Fatalf("pictures = %q", pics)
	}
	if labels[0] != "scan_0030.pdf, page 1" || labels[1] != "scan_0031.pdf, page 1" {
		t.Fatalf("labels = %q", labels)
	}
}

// pngHead is the start of a PNG file: page pictures in tests are pictures a model can be shown.
const pngHead = "\x89PNG\r\n\x1a\n"

// A page "picture" that is not one (a web page saved under .png) is never attached (GST suite T8, 2026-10-22).
func TestAPagePictureThatIsNotAPictureIsNotShown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "page-1.png")
	_ = os.WriteFile(p, []byte("<!DOCTYPE html>"), 0o644)
	st := &operatorState{Vision: true}
	if pics, _ := st.documentPictures("x.pdf: PDF\n[page image: " + p + "]\n"); len(pics) != 0 {
		t.Fatalf("pictures = %q", pics)
	}
}
