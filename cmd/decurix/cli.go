package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
)

// chatPollInterval is how often chat mode checks a desktop's transcript for
// what a running turn has done since the last check.
const chatPollInterval = time.Second

// apiClient is a thin HTTP client for the control plane's JSON API - the
// same one the dashboard and chat page use (internal/httpapi). It decodes
// straight into that package's own exported types, so the CLI has no second
// definition of what a desktop, a chat entry or a query note looks like.
type apiClient struct {
	baseURL string
	http    *http.Client
}

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// do sends one request and decodes its JSON body into out (nil to discard
// it). form is sent as the query string on a GET and as a
// application/x-www-form-urlencoded body otherwise, matching how every
// internal/httpapi handler above reads its parameters.
func (c *apiClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	target := c.baseURL + path

	var body io.Reader

	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}

	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("%s", apiErr.Error)
		}

		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	if out == nil {
		return nil
	}

	return json.Unmarshal(data, out)
}

func (c *apiClient) listDesktops(ctx context.Context) ([]desktop.Desktop, error) {
	var out []desktop.Desktop
	err := c.do(ctx, http.MethodGet, "/api/desktops", nil, &out)

	return out, err
}

func (c *apiClient) createDesktop(ctx context.Context, form url.Values) (desktop.Desktop, error) {
	var out desktop.Desktop
	err := c.do(ctx, http.MethodPost, "/api/desktops/create", form, &out)

	return out, err
}

// desktopAction is start/stop/restart/delete: each is a POST of just the
// desktop id to its own path (internal/httpapi/desktops.go).
func (c *apiClient) desktopAction(ctx context.Context, path, id string) error {
	return c.do(ctx, http.MethodPost, path, url.Values{"id": {id}}, nil)
}

func (c *apiClient) sendRequest(ctx context.Context, id, content string) error {
	return c.do(ctx, http.MethodPost, "/api/desktops/send", url.Values{"id": {id}, "content": {content}}, nil)
}

func (c *apiClient) chatSnapshot(ctx context.Context, id string) (conversation.Thread, error) {
	var out conversation.Thread
	err := c.do(ctx, http.MethodGet, "/api/desktops/chat", url.Values{"id": {id}}, &out)

	return out, err
}

func (c *apiClient) queryNotes(ctx context.Context) ([]querynotes.QueryNote, error) {
	var out []querynotes.QueryNote
	err := c.do(ctx, http.MethodGet, "/api/querynotes", nil, &out)

	return out, err
}

// runCLI is an interactive REPL against an already-running control plane,
// covering the same ground as the dashboard and a desktop's chat page:
// list/create/start/stop/restart/delete a desktop, send it a request and
// watch the reply, and read open query notes. It returns the process exit
// code.
func runCLI(baseURL string) int {
	client := newAPIClient(baseURL)
	ctx := context.Background()

	fmt.Printf("Decurix CLI - connected to %s\n", baseURL)
	fmt.Println(`Type "help" for commands, "exit" to quit.`)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for {
		fmt.Print("decurix> ")

		if !scanner.Scan() {
			fmt.Println()
			return 0
		}

		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		cmd, args := fields[0], fields[1:]

		switch cmd {
		case "help":
			printCLIHelp()

		case "exit", "quit":
			return 0

		case "list", "ls":
			cliList(ctx, client)

		case "create":
			cliCreate(ctx, client, args)

		case "start":
			cliSimpleAction(ctx, client, "/api/desktops/start", args, "started")

		case "pause":
			cliSimpleAction(ctx, client, "/api/desktops/pause", args, "paused")

		case "unpause", "wake":
			cliSimpleAction(ctx, client, "/api/desktops/unpause", args, "woken")

		case "stop":
			cliSimpleAction(ctx, client, "/api/desktops/stop", args, "stopped")

		case "restart":
			cliSimpleAction(ctx, client, "/api/desktops/restart", args, "restarted")

		case "delete":
			cliDelete(ctx, client, args, scanner)

		case "chat":
			cliChat(ctx, client, args, scanner)

		case "queries", "notes":
			cliQueries(ctx, client)

		default:
			fmt.Printf("unknown command: %s (try \"help\")\n", cmd)
		}
	}
}

func printCLIHelp() {
	fmt.Println(`Commands:
  list | ls                          list desktops
  create <name> [--profile basic|developer|ai] [--harness thebot|claude|ollama] [--cpus N] [--memory-mb N] [--copy-apps] [--apps chrome,gh,ssh]
  start <id>
  pause <id>                         freeze container CPU (0% CPU)
  unpause | wake <id>                thaw frozen container (< 1ms)
  stop <id>
  restart <id>
  delete <id> [--yes]
  chat <id>                          open a chat with a desktop
  queries | notes                    list open query notes
  help
  exit | quit`)
}

func cliList(ctx context.Context, c *apiClient) {
	desktops, err := c.listDesktops(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	if len(desktops) == 0 {
		fmt.Println("no desktops")
		return
	}

	rows := [][]string{{"ID", "NAME", "STATUS", "PROFILE", "HARNESS", "MODEL"}}
	for _, d := range desktops {
		rows = append(rows, []string{d.ID, d.Name, d.Status, d.Profile, d.Harness, d.Model})
	}

	printTable(rows)
}

// printTable prints rows (rows[0] is the header) as aligned columns, using
// only what a column actually needs - no external dependency for something
// this small.
func printTable(rows [][]string) {
	widths := make([]int, len(rows[0]))

	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Print("  ")
			}

			fmt.Print(cell)

			if i < len(row)-1 {
				fmt.Print(strings.Repeat(" ", widths[i]-len(cell)))
			}
		}

		fmt.Println()
	}
}

// createFlags maps a create command's flag spelling to the form field
// internal/httpapi/desktops.go's createDesktopHandler reads it as.
var createFlags = map[string]string{
	"--profile":    "profile",
	"--harness":    "harness",
	"--cpus":       "cpus",
	"--memory-mb":  "memory_mb",
	"--apps":       "host_apps",
	"--path":       "host_paths",
	"--paths":      "host_paths",
	"--copy-path":  "host_paths",
	"--copy-paths": "host_paths",
	"--host-path":  "host_paths",
	"--host-paths": "host_paths",
}

func cliCreate(ctx context.Context, c *apiClient, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: create <name> [--profile basic|developer|ai] [--harness thebot|claude|ollama] [--cpus N] [--memory-mb N] [--copy-apps] [--apps chrome,gh,ssh] [--path <host_path>[-><vm_path>]]")
		return
	}

	form := url.Values{"name": {args[0]}}

	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--copy-apps" || rest[i] == "--copy-host-apps" {
			form.Set("copy_host_apps", "1")
			continue
		}
		if rest[i] == "--copy-files" || rest[i] == "--copy-host-files" {
			form.Set("copy_host_files", "1")
			continue
		}

		field, ok := createFlags[rest[i]]
		if !ok {
			fmt.Println("unknown flag:", rest[i])
			return
		}

		i++
		if i >= len(rest) {
			fmt.Println("missing value for", rest[i-1])
			return
		}

		if field == "host_paths" {
			form.Add("host_paths", rest[i])
			form.Set("copy_host_files", "1")
		} else {
			form.Set(field, rest[i])
		}
		if field == "host_apps" {
			form.Set("copy_host_apps", "1")
		}
	}

	created, err := c.createDesktop(ctx, form)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("created %s (%s, %s/%s)\n", created.Name, created.ID, created.Profile, created.Harness)
}

func cliSimpleAction(ctx context.Context, c *apiClient, path string, args []string, verb string) {
	if len(args) != 1 {
		fmt.Println("usage:", strings.TrimPrefix(path, "/api/desktops/"), "<id>")
		return
	}

	id := args[0]

	if err := c.desktopAction(ctx, path, id); err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("%s %s\n", id, verb)
}

// cliDelete asks for confirmation the same way the dashboard's delete button
// does (web/templates/index.html's confirm dialog), unless told not to.
func cliDelete(ctx context.Context, c *apiClient, args []string, scanner *bufio.Scanner) {
	if len(args) == 0 {
		fmt.Println("usage: delete <id> [--yes]")
		return
	}

	id := args[0]
	confirmed := len(args) > 1 && args[1] == "--yes"

	if !confirmed {
		fmt.Printf("Type 'yes' to confirm deleting %s: ", id)

		if !scanner.Scan() {
			fmt.Println()
			return
		}

		if strings.TrimSpace(scanner.Text()) != "yes" {
			fmt.Println("aborted")
			return
		}
	}

	if err := c.desktopAction(ctx, "/api/desktops/delete", id); err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("%s deleted\n", id)
}

func cliQueries(ctx context.Context, c *apiClient) {
	notes, err := c.queryNotes(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	if len(notes) == 0 {
		fmt.Println("no open queries")
		return
	}

	for _, note := range notes {
		fmt.Printf("[%s] %s - %s\n", note.ID, note.DesktopID, note.CreatedAt.Format(time.RFC3339))
		fmt.Println(strings.TrimSpace(note.Content))
		fmt.Println()
	}
}

// cliChat opens a chat sub-mode with one desktop: it prints the transcript
// so far, then every non-slash line sent is delivered exactly as the chat
// page's send box would (POST /api/desktops/send) and waited out until the
// desktop replies.
func cliChat(ctx context.Context, c *apiClient, args []string, scanner *bufio.Scanner) {
	if len(args) != 1 {
		fmt.Println("usage: chat <id>")
		return
	}

	id := args[0]

	thread, err := c.chatSnapshot(ctx, id)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	entriesShown, stepsShown := renderChatDelta(thread, 0, 0)

	for {
		fmt.Printf("%s chat> ", id)

		if !scanner.Scan() {
			fmt.Println()
			return
		}

		line := strings.TrimSpace(scanner.Text())

		switch {
		case line == "":
			continue

		case line == "/exit":
			return

		case line == "/refresh":
			t, err := c.chatSnapshot(ctx, id)
			if err != nil {
				fmt.Println("error:", err)
				continue
			}

			entriesShown, stepsShown = renderChatDelta(t, entriesShown, stepsShown)

		default:
			if err := c.sendRequest(ctx, id, line); err != nil {
				fmt.Println("error:", err)
				continue
			}

			entriesShown, stepsShown = waitForReply(ctx, c, id, entriesShown, stepsShown, chatPollInterval)
		}
	}
}

// waitForReply polls the transcript until the desktop's current turn ends
// with at least one new entry, printing live steps and the reply as they
// arrive. Ctrl+C returns to the chat prompt without cancelling the task - it
// keeps running on the desktop, same as closing a browser tab would leave it.
func waitForReply(ctx context.Context, c *apiClient, id string, entriesShown, stepsShown int, interval time.Duration) (int, int) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	startEntries := entriesShown

	for {
		select {
		case <-interrupt:
			fmt.Println("\n(still working - use /refresh to check back)")
			return entriesShown, stepsShown

		case <-ticker.C:
			t, err := c.chatSnapshot(ctx, id)
			if err != nil {
				fmt.Println("error:", err)
				return entriesShown, stepsShown
			}

			entriesShown, stepsShown = renderChatDelta(t, entriesShown, stepsShown)

			if !t.Running && entriesShown > startEntries {
				return entriesShown, stepsShown
			}
		}
	}
}

// renderChatDelta prints whatever is new in t since entriesShown/stepsShown
// were last recorded, and returns the counts to pass back in next time.
// Steps are the live activity of a turn that has not become an entry yet
// (internal/conversation's Thread.Steps), so they are only worth printing
// while the turn is still running.
func renderChatDelta(t conversation.Thread, entriesShown, stepsShown int) (int, int) {
	for ; entriesShown < len(t.Entries); entriesShown++ {
		printChatEntry(t.Entries[entriesShown])
	}

	if t.Running {
		for ; stepsShown < len(t.Steps); stepsShown++ {
			fmt.Println("  ...", t.Steps[stepsShown].Text)
		}
	} else {
		stepsShown = len(t.Steps)
	}

	return entriesShown, stepsShown
}

func printChatEntry(e conversation.Entry) {
	label := "You"

	if e.Role == conversation.RoleAgent {
		switch e.Kind {
		case conversation.KindApproval:
			label = "Agent (needs approval)"
		case conversation.KindQuery:
			label = "Agent (needs your answer)"
		case conversation.KindError:
			label = "Agent (error)"
		default:
			label = "Agent"
		}
	}

	fmt.Printf("%s: %s\n", label, e.Content)
}
