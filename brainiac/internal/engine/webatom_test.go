package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
)

func TestFreshQuestionsGetWebEvidence(t *testing.T) {
	filler := strings.Repeat("<p>Weather was pleasant across the city all week, with light winds and clear evenings everywhere.</p>\n", 40)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>Repo rate news</title></head><body><main>`+filler+`
<p>The central bank kept the repo rate at 5.25% on 8 October 2026, its third pause in a row.</p>
</main></body></html>`)
	}))
	defer page.Close()
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("q"), "repo rate") || strings.Contains(r.URL.Query().Get("q"), "ABCDE1234F") {
			t.Errorf("query %q", r.URL.Query().Get("q"))
		}
		fmt.Fprintf(w, `<a class="result__a" href="%s/news">Repo rate news</a><a class="result__snippet" href="x">kept at 5.25%%</a>`, page.URL)
	}))
	defer search.Close()

	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL, cfg.Web = config.ModeLocal, srv.URL, config.WebAuto
	off := false
	cfg.WebRender = &off
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	e.web.AllowPrivate = true
	e.web.HTTP = nil
	e.web.Engines = map[string]string{"duckduckgo": search.URL, "bing": "http://127.0.0.1:1"}

	res, err := e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{
		{Kind: "text", Text: "What is the current repo rate? (client ABCDE1234F)"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := fmt.Sprint(fo.reqs[0]["messages"])
	if !strings.Contains(msgs, `kind="web"`) || !strings.Contains(msgs, "5.25%") || !strings.Contains(msgs, "[web-N]") {
		t.Fatalf("no web evidence sent:\n%s", msgs)
	}
	if strings.Contains(msgs, "Weather was pleasant") {
		t.Errorf("an unrelated paragraph was sent")
	}
	ops := ""
	for _, a := range res.Trace.Atoms {
		ops += a.Op + " "
	}
	if !strings.Contains(ops, "web-search") || !strings.Contains(ops, "web-fetch") {
		t.Fatalf("atoms %s", ops)
	}
	notes := strings.Join(res.Trace.Notes, " ")
	if !strings.Contains(notes, "read the web") || !strings.Contains(notes, "taken out of the web search query") {
		t.Fatalf("notes %q", notes)
	}

	// A timeless question reads nothing; the client can still force it off.
	fo.reqs = nil
	res, _ = e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "Explain photosynthesis"}}}}})
	for _, a := range res.Trace.Atoms {
		if strings.HasPrefix(a.Op, "web") {
			t.Fatal("a timeless question read the web")
		}
	}
	no := false
	res, _ = e.Chat(context.Background(), Request{Web: &no, Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "latest repo rate"}}}}})
	for _, a := range res.Trace.Atoms {
		if strings.HasPrefix(a.Op, "web") {
			t.Fatal("web: false was ignored")
		}
	}
}
