package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const article = `<!doctype html><html><head><title>Qwen3.9 released &amp; benchmarks</title>
<meta property="article:published_time" content="2026-10-20T09:00:00Z"><script>var x = "noise";</script><style>p{}</style></head>
<body><nav>Home | About | Login</nav><main><h1>Qwen3.9 released</h1>
<p>The Qwen team released Qwen3.9 on 20 October 2026 under Apache-2.0.</p>
<p>Unrelated paragraph about the weather in the hills, which has nothing to do with models at all.</p>
<table><tr><th>Model</th><th>AIME26</th></tr><tr><td>Qwen3.9-4B</td><td>91.2</td></tr></table>
</main><footer>Copyright</footer></body></html>`

func TestFetchReadsTheArticleNotThePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, article)
	}))
	defer srv.Close()
	c := New()
	c.AllowPrivate = true
	c.HTTP = c.client()
	p, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Qwen3.9 released & benchmarks" || p.Published != "2026-10-20T09:00:00Z" {
		t.Fatalf("title %q published %q", p.Title, p.Published)
	}
	for _, gone := range []string{"noise", "Login", "Copyright", "p{}"} {
		if strings.Contains(p.Text, gone) {
			t.Errorf("%q survived: %q", gone, p.Text)
		}
	}
	if !strings.Contains(p.Text, "Qwen3.9-4B | | 91.2") && !strings.Contains(p.Text, "Qwen3.9-4B") {
		t.Errorf("table lost: %q", p.Text)
	}
	got := Passages(p.Text, "when was qwen3.9 released and under which licence", 120)
	if !strings.Contains(got, "Apache-2.0") || strings.Contains(got, "weather") {
		t.Errorf("passages %q", got)
	}
}

func TestTheGuardNeverDialsInside(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret admin page") }))
	defer srv.Close()
	c := New() // AllowPrivate false: 127.0.0.1 is refused at dial time
	if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback fetched: %v", err)
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://x.org", "http://user:pw@example.com/", "javascript:alert(1)"} {
		if _, err := c.Fetch(context.Background(), bad); err == nil {
			t.Errorf("%s was fetched", bad)
		}
	}
	if _, err := c.Render(context.Background(), "http://127.0.0.1:1/"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("render of loopback: %v", err)
	}
}

func TestForbiddenIsRetriedOnceAsBrainiac(t *testing.T) {
	var uas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uas = append(uas, r.Header.Get("User-Agent"))
		if r.Header.Get("User-Agent") != UserAgent {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "<html><body><p>filed returns are listed here for everyone</p></body></html>")
	}))
	defer srv.Close()
	c := New()
	c.AllowPrivate = true
	c.HTTP = c.client()
	p, err := c.Fetch(context.Background(), srv.URL)
	if err != nil || !strings.Contains(p.Text, "filed returns") || len(uas) != 2 {
		t.Fatalf("page %q err %v agents %v", p.Text, err, uas)
	}
}

func TestSearchParsesDuckDuckGoAndDropsAds(t *testing.T) {
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div><a rel="nofollow" class="result__a" href="https://duckduckgo.com/y.js?ad=1">Ad</a><a class="result__snippet" href="x">buy</a></div>
<div><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fqwen.ai%2Fblog%2Fqwen3.9&amp;rut=1">Qwen3.9 <b>blog</b></a>
<a class="result__snippet" href="x">Released today under Apache-2.0</a></div>
<div><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fqwen.ai%2Fblog%2Fqwen3.9&amp;rut=2">dup</a><a class="result__snippet" href="x">dup</a></div>`)
	}))
	defer ddg.Close()
	c := New()
	c.AllowPrivate = true
	c.HTTP = c.client()
	c.Engines = map[string]string{"duckduckgo": ddg.URL, "bing": "http://127.0.0.1:1"}
	res, err := c.Search(context.Background(), "qwen3.9 release", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].URL != "https://qwen.ai/blog/qwen3.9" || res[0].Title != "Qwen3.9 blog" {
		t.Fatalf("results %+v", res)
	}
}

func TestScrubKeepsTheQuestionAndDropsIdentities(t *testing.T) {
	q, scrubbed := Scrub("GST rate for client ABCDE1234F GSTIN 27ABCDE1234F1Z5 mail me at ca@firm.in, password: hunter22 and token sk-abcdefghijklmnop1234")
	if !scrubbed {
		t.Fatal("nothing scrubbed")
	}
	for _, gone := range []string{"ABCDE1234F", "27ABCDE1234F1Z5", "ca@firm.in", "hunter22", "sk-abcdefgh"} {
		if strings.Contains(q, gone) {
			t.Errorf("%q left in %q", gone, q)
		}
	}
	if !strings.Contains(q, "GST rate for client") {
		t.Errorf("question lost: %q", q)
	}
	if q, scrubbed := Scrub("how do I reset my password in Tally"); scrubbed || q != "how do I reset my password in Tally" {
		t.Errorf("a plain question was changed: %q", q)
	}
}

func TestQueryFromKeepsOnlyTheClientsQuery(t *testing.T) {
	q, _ := QueryFrom("  repo rate   October 2026 ")
	if q != "repo rate October 2026" {
		t.Errorf("query %q", q)
	}
	long := strings.Repeat("word ", 80)
	if q, _ := QueryFrom(long); len(q) > 200 {
		t.Errorf("query not cut to 200: %d", len(q))
	}
}

func TestLiveSearchAndRender(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") == "" {
		t.Skip("set BRAINIAC_LIVE=1 to search the real web")
	}
	c := New()
	res, err := c.Search(context.Background(), "llama.cpp github releases", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		t.Logf("%s  %s", r.Engine, r.URL)
	}
	p, err := c.Fetch(context.Background(), res[0].URL)
	if err != nil {
		t.Logf("fetch %s: %v", res[0].URL, err)
	} else {
		t.Logf("fetched %q: %d chars", p.Title, len(p.Text))
	}
	if FindBrowser() != "" && os.Getenv("BRAINIAC_LIVE_RENDER") != "" {
		rp, err := c.Render(context.Background(), "https://huggingface.co/models?sort=trending")
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		t.Logf("rendered %q: %d chars", rp.Title, len(rp.Text))
	}
}

// The headless browser's proxy dials with Fetch's guard: a loopback target
// is refused over plain http and over CONNECT, and reached only when the
// guard is lifted (tests).
func TestTheBrowserProxyRefusesWhatFetchRefuses(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "inside") }))
	defer target.Close()
	for _, allow := range []bool{false, true} {
		c := New()
		c.AllowPrivate = allow
		addr, stop, err := c.startProxy()
		if err != nil {
			t.Fatal(err)
		}
		proxyURL, _ := url.Parse("http://" + addr)
		hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
		resp, err := hc.Get(target.URL)
		got := ""
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			got = string(b)
		}
		if allow != (got == "inside") {
			t.Errorf("allow %v: plain http got %q (%v)", allow, got, err)
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		host := strings.TrimPrefix(target.URL, "http://")
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
		line, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if allow != strings.Contains(line, " 200 ") {
			t.Errorf("allow %v: CONNECT answered %q", allow, line)
		}
		stop()
	}
}
