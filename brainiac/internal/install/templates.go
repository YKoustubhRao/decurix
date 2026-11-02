package install

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"brainiac/internal/gguf"
	"brainiac/internal/store"
)

// TemplatesFile is the record of each model's chat template, under the store root.
const TemplatesFile = "templates.json"

// TemplateRecord is one chat template seen in a model's file.
type TemplateRecord struct {
	SHA    string `json:"sha"` // "" for a file with no template
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	File   string `json:"file,omitempty"` // the template's text, to read
	Seen   string `json:"seen"`
	// By is how it came to be accepted: "first install" or "a person".
	By string `json:"by,omitempty"`
}

// TemplateBook is every model's accepted template, and the ones waiting for a person.
type TemplateBook struct {
	Accepted map[string]TemplateRecord `json:"accepted"`
	Waiting  map[string]TemplateRecord `json:"waiting,omitempty"`
}

// TemplateChanged is a model whose file carries a different chat template
// from the one accepted. A template runs on every request and can rewrite any
// prompt without showing on the model card, so only a person accepts a change.
type TemplateChanged struct {
	Model    string
	Old, New TemplateRecord
}

func (e *TemplateChanged) Error() string {
	return fmt.Sprintf("%s: the chat template changed (%s at %s, was %s at %s). Read %s, and accept it with `brainiac update approve-template %s`",
		e.Model, short(e.New.SHA), e.New.Repo, short(e.Old.SHA), e.Old.Repo, e.New.File, e.Model)
}

func short(sha string) string {
	if sha == "" {
		return "no template"
	}
	return sha[:min(12, len(sha))]
}

func readBook(st *store.Store) (TemplateBook, error) {
	var b TemplateBook
	if err := st.ReadJSON(TemplatesFile, &b); err != nil && !errors.Is(err, os.ErrNotExist) {
		return b, fmt.Errorf("%s: %w", TemplatesFile, err)
	}
	if b.Accepted == nil {
		b.Accepted = map[string]TemplateRecord{}
	}
	if b.Waiting == nil {
		b.Waiting = map[string]TemplateRecord{}
	}
	return b, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// checkTemplate reads the chat template of a model's GGUF file. The first one
// a model installs is recorded and accepted; after that a different one stops
// the install. It returns the template's sha, for the lock.
func checkTemplate(st *store.Store, model, repo, commit, path string, now time.Time) (string, error) {
	templates, err := gguf.Templates(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", model, err)
	}
	sha := gguf.SHA(templates)
	rec := TemplateRecord{SHA: sha, Repo: repo, Commit: commit, Seen: now.UTC().Format("2006-01-02")}
	if sha != "" {
		rec.File = st.Path("templates", unsafeName.ReplaceAllString(model, "_")+"-"+short(sha)+".jinja")
		var text []string
		for _, k := range sortedTemplateKeys(templates) {
			text = append(text, "{# "+k+" #}\n"+templates[k])
		}
		if err := os.MkdirAll(st.Path("templates"), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(rec.File, []byte(strings.Join(text, "\n\n")), 0o644); err != nil {
			return "", err
		}
	}
	book, err := readBook(st)
	if err != nil {
		return "", err
	}
	old, seen := book.Accepted[model]
	switch {
	case !seen:
		rec.By = "first install"
		book.Accepted[model] = rec
		delete(book.Waiting, model)
	case old.SHA != sha:
		book.Waiting[model] = rec
		if err := st.WriteJSON(TemplatesFile, book); err != nil {
			return "", err
		}
		return "", &TemplateChanged{Model: model, Old: old, New: rec}
	default:
		return sha, nil
	}
	return sha, st.WriteJSON(TemplatesFile, book)
}

func sortedTemplateKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ApproveTemplate is a person accepting the template waiting for a model.
func ApproveTemplate(st *store.Store, model string) (TemplateRecord, error) {
	book, err := readBook(st)
	if err != nil {
		return TemplateRecord{}, err
	}
	rec, ok := book.Waiting[model]
	if !ok {
		return TemplateRecord{}, fmt.Errorf("no changed chat template is waiting for %s", model)
	}
	rec.By = "a person"
	book.Accepted[model] = rec
	delete(book.Waiting, model)
	return rec, st.WriteJSON(TemplatesFile, book)
}
