package engine

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"brainiac/internal/cache"
	"brainiac/internal/config"
	"brainiac/internal/trace"
	"brainiac/internal/web"
)

// WebNote is added to the system prompt when an answer carries web evidence.
// Constant text, like EvidenceNote, so the prompt prefix stays stable.
const WebNote = `Some evidence comes from live web pages read just now (kind="web", with their address and the date they were read). For anything that may have changed since you were trained, trust it over your memory and cite it as [web-N]. If it does not answer the question, say so instead of guessing.`

// Limits on what the web adds to one request.
const (
	webResults      = 6
	webPagesRead    = 3
	webPassageChars = 2500
	webMinText      = 400 // a page with less text than this probably needs JavaScript
)

// wantsWeb decides whether a request reads the web. The second value says
// why it does, or why a lookup the client asked for did not happen.
//
// Nothing here reads the conversation: whether data leaves the machine is
// decided by what the client sent ("web" and "web_query") and the operator's
// setting, never by guessing from the words of a message.
func (e *Engine) wantsWeb(req Request, mode string) (bool, string) {
	if req.Web != nil && !*req.Web {
		return false, ""
	}
	asked := req.Web != nil && *req.Web
	query := strings.TrimSpace(req.WebQuery)
	if e.web == nil || e.cfg.Web == config.WebOff {
		if asked || query != "" {
			return false, "the web is off in this Brainiac's settings, so nothing was looked up"
		}
		return false, ""
	}
	// auto lets a web_query alone ask for a lookup, except for a local
	// answer, where only an explicit "web": true does (Validate refuses auto
	// in local mode).
	auto := e.cfg.Web == config.WebAuto && e.cfg.Mode != config.ModeLocal && mode != config.ModeLocal
	if !asked && !(auto && query != "") {
		return false, ""
	}
	if query == "" {
		return false, "the client asked for the web but sent no web_query; Brainiac never searches with the conversation's own words, so nothing was looked up"
	}
	if asked {
		return true, "the client asked for the web with its own query"
	}
	return true, "the client sent a web_query"
}

// webEvidence searches with the client's query, reads the best pages and
// returns them as evidence blocks web-1..N.
func (e *Engine) webEvidence(ctx context.Context, req Request, rec *trace.Record) []string {
	query, scrubbed := web.QueryFrom(req.WebQuery)
	if query == "" {
		rec.Notes = append(rec.Notes, "the web_query held nothing that may leave this machine, so nothing was looked up")
		return nil
	}
	if scrubbed {
		rec.Notes = append(rec.Notes, "identity numbers, contact details or secrets were taken out of the web search query")
	}
	if req.OnProgress != nil {
		req.OnProgress("searching the web")
	}
	hour := time.Now().UTC().Format("2006010215")
	t0 := time.Now()
	var results []web.Result
	key := cache.Key("web-search", "", hour, []byte(query))
	sa := trace.Atom{ID: "web-search", Op: "web-search"}
	if e.cache.Get(key, &results) {
		sa.Cached = true
	} else {
		var err error
		results, err = e.web.Search(ctx, query, webResults)
		if err != nil {
			sa.Error = err.Error()
			sa.Ms = time.Since(t0).Milliseconds()
			rec.Atoms = append(rec.Atoms, sa)
			return []string{missingEvidence("web-1", "web", "the web search failed")}
		}
		e.cache.Put(key, results)
	}
	if len(results) > 0 {
		sa.Model = results[0].Engine
	}
	sa.Ms, sa.OutSize = time.Since(t0).Milliseconds(), len(results)
	rec.Atoms = append(rec.Atoms, sa)

	read := min(webPagesRead, len(results))
	pages := make([]*web.Page, read)
	atoms := make([]trace.Atom, read)
	var wg sync.WaitGroup
	for i := 0; i < read; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pages[i], atoms[i] = e.readPage(ctx, results[i].URL, hour)
		}(i)
	}
	wg.Wait()
	rec.Atoms = append(rec.Atoms, atoms...)

	retrieved := time.Now().UTC().Format("2006-01-02")
	var out []string
	for i, r := range results {
		id := fmt.Sprintf("web-%d", i+1)
		if i < read && pages[i] != nil {
			p := pages[i]
			title := p.Title
			if title == "" {
				title = r.Title
			}
			pub := ""
			if p.Published != "" {
				pub = fmt.Sprintf(" published=%q", p.Published)
			}
			out = append(out, fmt.Sprintf("<evidence id=%q kind=\"web\" url=%q title=%q retrieved=%q%s>\n%s\n</evidence>",
				id, inert(p.URL), inert(title), retrieved, inert(pub), inert(web.Passages(p.Text, query, webPassageChars))))
			continue
		}
		// Not read: its search snippet is still a lead.
		out = append(out, fmt.Sprintf("<evidence id=%q kind=\"web\" url=%q title=%q retrieved=%q snippet-only=\"true\">\n%s\n</evidence>",
			id, inert(r.URL), inert(r.Title), retrieved, inert(r.Snippet)))
	}
	return out
}

// readPage fetches one result, through the cache, with the headless browser
// when the plain page has almost no text.
func (e *Engine) readPage(ctx context.Context, link, hour string) (*web.Page, trace.Atom) {
	t0 := time.Now()
	host := link
	if u, err := url.Parse(link); err == nil {
		host = u.Host
	}
	a := trace.Atom{ID: "web-fetch", Op: "web-fetch", Model: host}
	key := cache.Key("web-page", "", hour[:8]+fmt.Sprint(time.Now().UTC().Hour()/6), []byte(link))
	var p web.Page
	if e.cache.Get(key, &p) {
		a.Cached, a.Ms, a.OutSize = true, time.Since(t0).Milliseconds(), len(p.Text)
		return &p, a
	}
	p, err := e.web.Fetch(ctx, link)
	if (err != nil || len(p.Text) < webMinText) && e.cfg.Render() && web.FindBrowser() != "" {
		if rp, rerr := e.web.Render(ctx, link); rerr == nil && len(rp.Text) > len(p.Text) {
			p, err = rp, nil
			a.Runtime = "headless-browser"
		}
	}
	a.Ms = time.Since(t0).Milliseconds()
	if err != nil {
		a.Error = err.Error()
		return nil, a
	}
	if strings.TrimSpace(p.Text) == "" {
		a.Error = "no readable text"
		return nil, a
	}
	a.OutSize = len(p.Text)
	e.cache.Put(key, p)
	return &p, a
}
