package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"thebot/internal/llm"
)

// FileWriteSkill writes real files to disk. The whole point, and the
// reason it was ported back after the Go rewrite shipped without it:
// "write the code into a file" should produce a FILE, not a wall of code
// printed back into the chat. The answer it returns is a short
// confirmation naming the real path - the content lives on disk.
type FileWriteSkill struct{}

func NewFileWrite() *FileWriteSkill { return &FileWriteSkill{} }

func (f *FileWriteSkill) Name() string { return "file-write" }
func (f *FileWriteSkill) Description() string {
	return "Writes real content to a real file in the workspace - verified on disk, never just printed back."
}

func (f *FileWriteSkill) Matches(subtask Subtask) bool { return subtask.Type == "file-write" }

var (
	fencedCodeRe = regexp.MustCompile("(?s)```[a-zA-Z0-9+#.-]*\n(.*?)```")
	pathHintRe   = regexp.MustCompile(`(?i)\b(?:in|into|to|named|called|as|file)\s+["'` + "`" + `]?([\w./-]+\.[a-zA-Z0-9]{1,8})["'` + "`" + `]?`)
	bareFileRe   = regexp.MustCompile(`\b([\w-]+(?:/[\w-]+)*\.[a-zA-Z0-9]{1,8})\b`)
)

const filePlanPrompt = `The user asked for a file to be written. Produce the file.

Respond in EXACTLY this format, nothing else:
PATH: <relative path for the file, e.g. code/segment_tree.py - infer a sensible name and folder if the user didn't give one>
CONTENT:
<the complete file content, raw, with no markdown fences>

The content must be complete and final. Never leave placeholders like [insert date] or "as of my last update" - if you were given findings below, they are the current truth and you must use them.

Request: %s`

// priorFindingsBlock is prepended when earlier steps in the plan found
// something. It is phrased as an override because the failure mode is a
// model quietly preferring its training data: told "write a note about
// the newest Go version" right after a search returned 1.27.1, it wrote
// "Go 1.21, as of my last update".
const priorFindingsBlock = `Earlier steps in this task already established the following. Treat these findings as current, authoritative fact - they come from real sources checked just now, and they override anything you remember from training:

%s

---
`

// planFile asks the model for the real path + content. A model call is
// genuinely needed here: only the model can turn "write a segment tree in
// python" into actual code, unlike the path/content already being present
// in the request.
func planFile(rc RunContext, step string) (path, content string, err error) {
	prompt := fmt.Sprintf(filePlanPrompt, step)
	if findings := rc.PriorFindings(); findings != "" {
		rc.Logf("file-write", "using findings from %d earlier step(s)", len(rc.PriorResults))
		prompt = fmt.Sprintf(priorFindingsBlock, findings) + prompt
	}

	resp, callErr := rc.Provider.Chat(rc.Ctx, rc.Model,
		[]llm.Message{{Role: "user", Content: prompt}},
		llm.Options{TimeoutMs: 120000})
	if callErr != nil {
		return "", "", callErr
	}

	text := resp.Text
	idx := strings.Index(text, "CONTENT:")
	if idx == -1 {
		return "", "", fmt.Errorf("model did not return the expected PATH/CONTENT format")
	}

	header := text[:idx]
	content = strings.TrimLeft(text[idx+len("CONTENT:"):], "\r\n")

	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "PATH:") {
			path = strings.TrimSpace(line[strings.Index(line, ":")+1:])
			path = strings.Trim(path, "`\"' ")
		}
	}

	// If the model wrapped the content in a fence despite being asked not
	// to, unwrap it - a file whose first line is ```python is broken.
	if m := fencedCodeRe.FindStringSubmatch(content); m != nil {
		content = m[1]
	}
	content = strings.TrimRight(content, "` \n\r") + "\n"

	if path == "" {
		return "", "", fmt.Errorf("model did not give a file path")
	}
	return path, content, nil
}

// pathFromRequest pulls an explicit filename out of the request when the
// user actually named one, so their choice wins over the model's guess.
func pathFromRequest(step string) string {
	if m := pathHintRe.FindStringSubmatch(step); m != nil {
		return m[1]
	}
	if m := bareFileRe.FindStringSubmatch(step); m != nil {
		return m[1]
	}
	return ""
}

func (f *FileWriteSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	if rc.Workspace == nil {
		return Result{Subtask: subtask, SkillName: f.Name(), Output: "No workspace configured.", Verified: false, Meta: map[string]any{}}, nil
	}

	rc.Logf("file-write", "planning the file to write...")
	path, content, err := planFile(rc, subtask.Step)
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: f.Name(),
			Output:   "Could not work out what file to write: " + err.Error(),
			Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}

	// The user's own filename wins over the model's invention.
	if explicit := pathFromRequest(subtask.Step); explicit != "" {
		path = explicit
	}

	abs, err := rc.Workspace.ResolveSafe(path)
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: f.Name(),
			Output:   "Refused to write outside the workspace: " + err.Error(),
			Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{Subtask: subtask, SkillName: f.Name(), Output: "Could not create the folder: " + err.Error(), Verified: false, Meta: map[string]any{"error": err.Error()}}, nil
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return Result{Subtask: subtask, SkillName: f.Name(), Output: "Could not write the file: " + err.Error(), Verified: false, Meta: map[string]any{"error": err.Error()}}, nil
	}

	// Verified against the real filesystem, never assumed from a
	// successful-looking write call.
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return Result{Subtask: subtask, SkillName: f.Name(), Output: "Wrote the file but could not verify it on disk: " + statErr.Error(), Verified: false, Meta: map[string]any{"error": statErr.Error()}}, nil
	}

	rc.Logf("file-write", "wrote %s (%d bytes) - verified on disk", abs, info.Size())

	lines := strings.Count(content, "\n")
	return Result{
		Subtask: subtask, SkillName: f.Name(),
		Output:   fmt.Sprintf("Wrote %s (%d lines, %d bytes).", abs, lines, info.Size()),
		Verified: true,
		Meta:     map[string]any{"path": abs, "relPath": path, "bytes": info.Size(), "lines": lines},
	}, nil
}
