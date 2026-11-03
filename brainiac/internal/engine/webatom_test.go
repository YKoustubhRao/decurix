package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	cfg.Mode, cfg.OllamaURL, cfg.Web = config.ModeLocal, srv.URL, config.WebOnRequest
	off := false
	cfg.WebRender = &off
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	e.web.AllowPrivate = true
	e.web.HTTP = nil
	e.web.Engines = map[string]string{"duckduckgo": search.URL, "bing": "http://127.0.0.1:1"}

	yes := true
	res, err := e.Chat(context.Background(), Request{Web: &yes, WebQuery: "current repo rate ABCDE1234F", Messages: []InMessage{{Role: "user", Parts: []Part{
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

	// Without "web": true nothing is read, whatever the message says; the
	// client can always force it off.
	fo.reqs = nil
	res, _ = e.Chat(context.Background(), Request{WebQuery: "latest repo rate", Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "latest repo rate today"}}}}})
	if usedWeb(res) {
		t.Fatal("on-request read the web without web: true")
	}
	no := false
	res, _ = e.Chat(context.Background(), Request{Web: &no, WebQuery: "latest repo rate", Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "latest repo rate"}}}}})
	if usedWeb(res) {
		t.Fatal("web: false was ignored")
	}
}

func usedWeb(res *Response) bool {
	for _, a := range res.Trace.Atoms {
		if strings.HasPrefix(a.Op, "web") {
			return true
		}
	}
	return false
}

// failingSearch is a search engine that fails the test if anything reaches it.
func failingSearch(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a search left the machine: %q", r.URL.RawQuery)
		http.Error(w, "no", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webEngine(t *testing.T, cfg config.Config, search string) *Engine {
	off := false
	cfg.WebRender = &off
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	e.web.AllowPrivate = true
	e.web.HTTP = nil
	e.web.Engines = map[string]string{"duckduckgo": search, "bing": search, "brave": search}
	return e
}

// Office text that names this year, "today" or "results for" stays on the
// machine under the default settings: no message text is ever a query.
func TestOfficeTextNeverLeavesUnderTheDefaults(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web != config.WebOnRequest {
		t.Fatalf("default web is %q, want on-request", cfg.Web)
	}
	cfg.OllamaURL = srv.URL
	e := webEngine(t, cfg, failingSearch(t).URL)
	year := time.Now().Format("2006")
	for _, text := range []string{
		"Client: Sharma Textiles Pvt Ltd, invoice INV/118 dated 03-11-" + year + ", amount Rs 4,50,000 to Mehta & Sons, Surat. Check the GST.",
		"Prepare GSTR-1 for FY " + year + " for Kapoor Electricals, turnover 2.3 crore",
		"The tax results for Anil Gupta (salary 18 lakh) are attached; summarise",
		"Draft a reminder to Ravi Verma that his loan EMI of 25,000 is due today",
	} {
		res, err := e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: text}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if usedWeb(res) {
			t.Errorf("read the web for %q", text)
		}
	}
}

// "web": true with no query of the client's own makes no outbound call, and
// the trace says why.
func TestWebWithoutAClientQueryLooksNothingUp(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	for _, mode := range []string{config.WebOnRequest, config.WebAuto} {
		cfg := config.Default()
		cfg.Mode, cfg.OllamaURL, cfg.Web = config.ModeHybrid, srv.URL, mode
		cfg.Remote = &config.Remote{Kind: config.RemoteOpenAI, URL: srv.URL}
		e := webEngine(t, cfg, failingSearch(t).URL)
		yes := true
		res, err := e.Chat(context.Background(), Request{Model: ModelLocal, Web: &yes, Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "latest repo rate for Kapoor Electricals"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if usedWeb(res) {
			t.Errorf("%s: a lookup ran with no web_query", mode)
		}
		if !strings.Contains(strings.Join(res.Trace.Notes, " "), "no web_query") {
			t.Errorf("%s: the trace does not say why nothing was looked up: %q", mode, res.Trace.Notes)
		}
	}
}

// In local mode a web_query alone never reads the web, even if the setting
// says auto (which Validate refuses there): only an explicit "web": true.
func TestLocalModeNeedsAnExplicitWebTrue(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL, cfg.Web = config.ModeLocal, srv.URL, config.WebAuto
	e := webEngine(t, cfg, failingSearch(t).URL)
	res, err := e.Chat(context.Background(), Request{WebQuery: "repo rate", Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "repo rate"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if usedWeb(res) {
		t.Error("local mode read the web without web: true")
	}
}
