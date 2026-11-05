package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"brainiac/internal/engine"
	"brainiac/internal/evals"
	"brainiac/internal/store"
)

// runEval measures models on checked cases through a running Brainiac and
// records the scores, field by field, in <home>/evals.json - which the server
// reads to decide when a specialist may take a field from the brain or the
// remote brain.
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	casesPath := fs.String("cases", "", "JSON-lines file of checked cases (domain, prompt, check)")
	modelsFlag := fs.String("models", "", "models to measure, comma-separated (default: the brain, each specialist for the cases' fields, and the remote brain)")
	url := fs.String("url", "http://127.0.0.1:8484", "Brainiac's address")
	keyEnv := fs.String("key-env", "", "environment variable holding the API key")
	home := fs.String("home", "", "Brainiac's home (where evals.json is kept)")
	learn := fs.Bool("learn", false, "also teach the router the cases' fields")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *casesPath == "" {
		return errors.New("eval needs --cases <file.jsonl>; each line: {\"domain\":\"math\",\"prompt\":\"...\",\"check\":{\"kind\":\"number\",\"value\":\"42\"}}")
	}
	cases, err := evals.ReadCases(*casesPath)
	if err != nil {
		return err
	}
	root := *home
	if root == "" {
		root = store.DefaultRoot()
	}
	st, err := store.Open(root)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cl := &evalClient{url: strings.TrimRight(*url, "/"), key: os.Getenv(*keyEnv)}

	domains := map[string]bool{}
	for _, c := range cases {
		domains[c.Domain] = true
	}
	plan, err := cl.defaultModels(ctx, domains)
	if err != nil {
		return err
	}
	if *modelsFlag != "" {
		plan = map[string][]string{}
		for _, m := range strings.Split(*modelsFlag, ",") {
			plan[strings.TrimSpace(m)] = nil // nil: every field
		}
	}
	if len(plan) == 0 {
		return errors.New("no model to measure: is Brainiac serving at " + *url + "?")
	}

	resultsPath := st.Path("evals.json")
	results, err := evals.Load(resultsPath)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(plan))
	for m := range plan {
		names = append(names, m)
	}
	sort.Strings(names)
	type tally struct {
		n, ok, prompt, completion int
		decided, decidedOK        int // cases a stated value decides (Check.Decides)
		ms                        int64
	}
	scores := map[string]map[string]*tally{}
	answered := map[string]map[int]bool{} // model -> case index -> correct
	meanMs := map[string]map[string]int64{}
	for _, model := range names {
		answered[model] = map[int]bool{}
		only := map[string]bool{}
		for _, d := range plan[model] {
			only[d] = true
		}
		scores[model] = map[string]*tally{}
		for ci, c := range cases {
			if len(only) > 0 && !only[c.Domain] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			t0 := time.Now()
			answer, usage, err := cl.ask(ctx, model, c)
			ms := time.Since(t0).Milliseconds()
			ok := err == nil && c.Check.Correct(answer)
			mark := "ok  "
			if !ok {
				mark = "MISS"
			}
			if err != nil {
				mark = "ERR "
			}
			fmt.Printf("%s %-22s %-10s %-14s %6.1fs", mark, model, c.Domain, c.ID, float64(ms)/1000)
			if err != nil {
				fmt.Printf("  %v", err)
			}
			fmt.Println()
			s := scores[model][c.Domain]
			if s == nil {
				s = &tally{}
				scores[model][c.Domain] = s
			}
			answered[model][ci] = ok
			s.n++
			s.ms += ms
			s.prompt += usage.PromptTokens
			s.completion += usage.CompletionTokens
			if ok {
				s.ok++
			}
			if c.Check.Decides() {
				s.decided++
				if ok {
					s.decidedOK++
				}
			}
		}
	}

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tFIELD\tCORRECT\tACCURACY\tCAN CLAIM\tMEAN TIME\tMEAN TOKENS")
	for _, model := range names {
		var ds []string
		for d := range scores[model] {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		for _, d := range ds {
			s := scores[model][d]
			score := evals.Score{N: s.n, Correct: s.ok, MeanMs: s.ms / int64(s.n), MeanPrompt: s.prompt / s.n, MeanCompletion: s.completion / s.n, At: time.Now().UTC(),
				Decided: s.decided, DecidedCorrect: s.decidedOK}
			if meanMs[model] == nil {
				meanMs[model] = map[string]int64{}
			}
			meanMs[model][d] = score.MeanMs
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%.0f%%\t%.0f%%\t%.1fs\t%d+%d\n", model, d, s.ok, s.n, 100*score.Accuracy(), 100*score.LowerBound(), float64(s.ms)/float64(s.n)/1000, score.MeanPrompt, score.MeanCompletion)
			results.Set(model, d, score)
		}
	}
	tw.Flush()

	// The routed path: where the router sends each case, answered by each
	// local model. A case sent to a field it is not from counts as a miss
	// there, and so does one the model was not asked.
	routed := make([]string, len(cases))
	for i, c := range cases {
		d, ok, err := cl.route(ctx, c.Prompt)
		if err != nil {
			return fmt.Errorf("asking the router (a Brainiac older than this eval?): %w", err)
		}
		if ok {
			routed[i] = d
		}
	}
	fmt.Println()
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROUTED PATH\tFIELD\tCORRECT\tCAN CLAIM\tSENT FROM OTHER FIELDS")
	for _, model := range names {
		if model == engine.RemoteEvalName {
			continue
		}
		for _, sc := range routedScores(cases, routed, answered[model]) {
			sc.score.MeanMs, sc.score.At = meanMs[model][sc.field], time.Now().UTC()
			results.Set(engine.RoutedEvalName(model), sc.field, sc.score)
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%.0f%%\t%d\n", model, sc.field, sc.score.Correct, sc.score.N, 100*sc.score.LowerBound(), sc.misrouted)
		}
	}
	tw.Flush()
	if err := results.Save(resultsPath); err != nil {
		return err
	}
	fmt.Printf("\nSaved to %s. A local model takes a field from the remote brain only with at least %d cases there that a stated number, letter or exact text decides (contains and regex cases are scored but prove nothing), when the accuracy it can claim is at least the remote brain's. `brainiac paths` shows where that leaves each field.\n", resultsPath, evals.MinCases)
	if *learn {
		n, err := learnRouter(st, cases)
		if err != nil {
			return err
		}
		fmt.Printf("The router learned %d new examples (%s).\n", n, st.Path(engine.RouterExamplesFile))
	}
	return nil
}

// routedScore is one model's routed-path score in one field.
type routedScore struct {
	field     string
	score     evals.Score
	misrouted int
}

// routedScores scores a model's routed path per field: every case the router
// sent to the field counts, and is correct only when the model answered it
// correctly. A case of another field sent there is a miss unless the model
// was asked it and got it right (the brain answers every field).
func routedScores(cases []evals.Case, routed []string, answered map[int]bool) []routedScore {
	by := map[string]*routedScore{}
	var order []string
	for i, c := range cases {
		d := routed[i]
		if d == "" {
			continue
		}
		s := by[d]
		if s == nil {
			s = &routedScore{field: d}
			by[d] = s
			order = append(order, d)
		}
		ok := answered[i]
		s.score.N++
		if ok {
			s.score.Correct++
		}
		if c.Check.Decides() {
			s.score.Decided++
			if ok {
				s.score.DecidedCorrect++
			}
		}
		if c.Domain != d {
			s.misrouted++
		}
	}
	sort.Strings(order)
	out := make([]routedScore, 0, len(order))
	for _, d := range order {
		out = append(out, *by[d])
	}
	return out
}

// route asks the server which field the router sends a text to.
func (c *evalClient) route(ctx context.Context, text string) (string, bool, error) {
	data, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/brainiac/route", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Domain string `json:"domain"`
		Routes bool   `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	return out.Domain, out.Routes, nil
}

// evalUsage is the tokens one answer used.
type evalUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type evalClient struct {
	url string
	key string
}

// defaultModels asks the server what is installed: the brain answers every
// field, a specialist only its own, and the remote brain every field.
func (c *evalClient) defaultModels(ctx context.Context, domains map[string]bool) (map[string][]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/v1/models", nil)
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Brainiac is not answering at %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID    string   `json:"id"`
			Roles []string `json:"roles"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, m := range list.Data {
		switch {
		case m.ID == engine.RemoteEvalName:
			out[m.ID] = nil
		case strings.HasPrefix(m.ID, "brainiac"):
			// virtual names route; measure concrete models only
		default:
			for _, r := range m.Roles {
				if r == "brain" {
					out[m.ID] = nil
				}
				if domains[r] {
					out[m.ID] = append(out[m.ID], r)
				}
			}
		}
	}
	return out, nil
}

func (c *evalClient) auth(r *http.Request) {
	if c.key != "" {
		r.Header.Set("Authorization", "Bearer "+c.key)
	}
}

// ask sends one case to one model and returns the answer text.
func (c *evalClient) ask(ctx context.Context, model string, cs evals.Case) (string, evalUsage, error) {
	var msgs []map[string]any
	if cs.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": cs.System})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": cs.Prompt})
	body := map[string]any{"model": model, "messages": msgs}
	if cs.MaxTokens > 0 {
		body["max_tokens"] = cs.MaxTokens
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", evalUsage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", evalUsage{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage evalUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", evalUsage{}, errors.New("the answer was not a chat completion")
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}

// learnRouter appends the cases' prompts and fields to the router's examples,
// skipping ones it already has.
func learnRouter(st *store.Store, cases []evals.Case) (int, error) {
	path := st.Path(engine.RouterExamplesFile)
	have := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ex struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(sc.Bytes(), &ex) == nil {
				have[ex.Text] = true
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	for _, c := range cases {
		if have[c.Prompt] {
			continue
		}
		have[c.Prompt] = true
		line, _ := json.Marshal(map[string]string{"text": c.Prompt, "domain": c.Domain})
		if _, err := f.Write(append(line, '\n')); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
