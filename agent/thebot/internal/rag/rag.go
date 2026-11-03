// Package rag is the Data RAG - real file content, extracted, chunked,
// embedded, and stored so the agent can retrieve grounded answers instead
// of guessing from a filename. This is the minimal REAL version: SQLite
// (pure-Go driver, no CGO - this dev environment has no C compiler, and
// it keeps the Docker image simpler too) at <workspaceRoot>/.rag/data.db,
// brute-force cosine similarity over stored chunks (entirely adequate at
// the scale a single VM's workspace produces).
//
// Extraction: PDF/DOCX/PPTX/XLSX/HTML go through scripts/docling_convert.py
// (real docling) via a Python subprocess - Go can't run docling itself,
// it's Python-only. Plain text/code files are read directly. Formats with
// no useful text representation are skipped, not force-fed through
// either path.
package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"thebot/internal/llm"
	"thebot/internal/migrate"
	"thebot/internal/withheld"
)

const (
	chunkSize    = 1500
	chunkOverlap = 200
)

// embedModel is deliberately empty - each llm.Embedder implementation
// owns its own default model (OllamaProvider.EmbedModel,
// OpenAIProvider.EmbedModel), so this package stays provider-agnostic
// rather than hardcoding a model name that would be wrong for anything
// but Ollama.
const embedModel = ""

var doclingExts = map[string]bool{
	".pdf": true, ".docx": true, ".doc": true, ".pptx": true, ".ppt": true,
	".xlsx": true, ".xls": true, ".html": true, ".htm": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".tif": true, ".tiff": true,
}

var skipExts = map[string]bool{
	".ico": true, ".zip": true, ".tar": true, ".gz": true, ".7z": true, ".rar": true,
	".exe": true, ".bin": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".mp3": true, ".wav": true, ".mp4": true, ".mov": true, ".avi": true,
	".pyc": true, ".pyo": true, ".class": true, ".o": true, ".so": true, ".db": true,
}

type Result struct {
	RelPath string
	Text    string
	Score   float64
}

type RAG struct {
	db            *sql.DB
	workspaceRoot string
	embedder      llm.Embedder
	doclingScript string
}

// Migrate is the Data RAG's schema, step by step: appended only, never edited (internal/migrate).
var Migrate = []string{
	`CREATE TABLE IF NOT EXISTS chunks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	rel_path TEXT NOT NULL,
	chunk_index INTEGER NOT NULL,
	text TEXT NOT NULL,
	embedding TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_relpath ON chunks(rel_path);`,
}

// New opens (creating if needed) the real SQLite-backed Data RAG at
// <workspaceRoot>/.rag/data.db.
func New(workspaceRoot string, embedder llm.Embedder, doclingScript string) (*RAG, error) {
	dir := filepath.Join(workspaceRoot, ".rag")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("rag: create dir: %w", err)
	}
	dbPath := filepath.Join(dir, "data.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("rag: open db: %w", err)
	}
	// Single-writer discipline for now (this process is the only writer) -
	// matches the "exclusive write access" requirement even before a
	// separate ingestor process exists.
	db.SetMaxOpenConns(1)

	if err := migrate.Migrate(db, Migrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("rag: %w", err)
	}

	return &RAG{db: db, workspaceRoot: workspaceRoot, embedder: embedder, doclingScript: doclingScript}, nil
}

func (r *RAG) Close() error { return r.db.Close() }

func extractText(ctx context.Context, absPath, doclingScript string) (text, method string, err error) {
	ext := strings.ToLower(filepath.Ext(absPath))
	if skipExts[ext] {
		return "", "", fmt.Errorf("skip: %s has no useful text representation", ext)
	}
	if doclingExts[ext] {
		ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "python3", doclingScript, absPath).Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			return "", "", fmt.Errorf("docling failed: %w (%s) - real output was: %s", err, stderr, string(out))
		}
		markdown, perr := parseDoclingOutput(out)
		if perr != nil {
			return "", "", perr
		}
		return markdown, "docling", nil
	}
	// A defense-in-depth backstop beyond the extension lists above, for
	// whatever they don't (and can't exhaustively) name - an extension-
	// less binary is the exact shape that slipped through live (a venv's
	// own compiled python3 executable, no extension at all, read whole
	// and chunked as if it were prose). Peek at a bounded prefix rather
	// than reading a potentially huge binary fully into memory just to
	// reject it.
	f, err := os.Open(absPath)
	if err != nil {
		return "", "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	peek := make([]byte, binarySniffBytes)
	n, err := f.Read(peek)
	if err != nil && err != io.EOF {
		return "", "", fmt.Errorf("read file: %w", err)
	}
	if looksBinary(peek[:n]) {
		return "", "", fmt.Errorf("skip: looks like binary content, not text")
	}
	if st, err := f.Stat(); err == nil && st.Size() > maxIngestBytes {
		return "", "", fmt.Errorf("skip: %d bytes is more than the %d a file is indexed up to", st.Size(), maxIngestBytes)
	}

	raw, err := io.ReadAll(io.LimitReader(f, maxIngestBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("read file: %w", err)
	}
	// The peek above already read the first bytes: the reader goes on from there.
	return string(peek[:n]) + string(raw), "text", nil
}

// maxIngestBytes bounds a plain-text file indexed whole (review T-17): read into memory and embedded chunk by chunk, a
// huge log would hold memory and the embedder for an hour on the target host.
const maxIngestBytes = 4 << 20

// binarySniffBytes is how much of a file gets checked for binary content -
// enough to reliably catch real binaries (their signature bytes are always
// at the very start), small enough to stay cheap even on huge files.
const binarySniffBytes = 8000

// looksBinary is the standard "does this contain a NUL byte" heuristic -
// the same one git, grep -I, and most other tools use to tell binary
// content from text without needing to know every possible file format.
// Genuine text essentially never contains a NUL byte; essentially every
// real binary format does within its first few hundred bytes.
func looksBinary(sample []byte) bool {
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	return false
}

func chunkText(text string) []string {
	var chunks []string
	runes := []rune(text)
	start := 0
	for start < len(runes) {
		end := min(start+chunkSize, len(runes))
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
		start = end - chunkOverlap
	}
	return chunks
}

// IngestFile extracts, chunks, embeds, and stores one real workspace
// file. Re-ingesting (file changed) replaces its old chunks entirely.
func (r *RAG) IngestFile(ctx context.Context, relPath string) (chunks int, method string, err error) {
	absPath := filepath.Join(r.workspaceRoot, relPath)
	if _, statErr := os.Stat(absPath); statErr != nil {
		return 0, "", fmt.Errorf("rag: %s is not a real file: %w", relPath, statErr)
	}

	text, method, err := extractText(ctx, absPath, r.doclingScript)
	if err != nil {
		return 0, "", err
	}

	// The job's secret values never reach the index, the embedder or a later recall (C14): withheld before chunking.
	pieces := chunkText(withheld.From(ctx).Text(text))
	if len(pieces) == 0 {
		return 0, method, nil
	}

	// Every chunk is embedded first, then written in one short transaction: on the one-connection pool a transaction
	// held across the embedding calls blocked every other use of the index (Stats, a query) for the whole ingest
	// (review T-17).
	vectors := make([]string, len(pieces))
	for i, piece := range pieces {
		vec, embErr := r.embedder.Embed(ctx, embedModel, piece)
		if embErr != nil {
			return 0, "", fmt.Errorf("rag: embed chunk %d of %s: %w", i, relPath, embErr)
		}
		vectors[i] = llm.EncodeVector(vec)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", fmt.Errorf("rag: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM chunks WHERE rel_path = ?`, relPath); err != nil {
		return 0, "", fmt.Errorf("rag: clear old chunks: %w", err)
	}

	now := time.Now().Unix()
	for i, piece := range pieces {
		if _, err := tx.Exec(
			`INSERT INTO chunks (rel_path, chunk_index, text, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
			relPath, i, piece, vectors[i], now,
		); err != nil {
			return 0, "", fmt.Errorf("rag: insert chunk %d of %s: %w", i, relPath, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, "", fmt.Errorf("rag: commit: %w", err)
	}
	return len(pieces), method, nil
}

// Query performs real brute-force cosine similarity over every stored
// chunk - grounded results only, never an invented file or excerpt.
func (r *RAG) Query(ctx context.Context, query string, topK int) ([]Result, error) {
	queryVec, err := r.embedder.Embed(ctx, embedModel, query)
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `SELECT rel_path, text, embedding FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("rag: query chunks: %w", err)
	}
	defer rows.Close()

	var results []Result
	for rows.Next() {
		var relPath, text, embStr string
		if err := rows.Scan(&relPath, &text, &embStr); err != nil {
			return nil, fmt.Errorf("rag: scan row: %w", err)
		}
		results = append(results, Result{RelPath: relPath, Text: text, Score: llm.CosineSimilarity(queryVec, llm.DecodeVector(embStr))})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// Stats reports real, checkable numbers - not a guess about what's indexed.
func (r *RAG) Stats(ctx context.Context) (totalChunks int, totalFiles int, err error) {
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&totalChunks); err != nil {
		return 0, 0, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT rel_path) FROM chunks`).Scan(&totalFiles); err != nil {
		return 0, 0, err
	}
	return totalChunks, totalFiles, nil
}

type FileStat struct {
	RelPath string
	Chunks  int
}

// ListFiles reports every real file actually indexed and its real chunk
// count - "how many documents" answered with the real list, not just a
// number, most-recently-ingested first.
func (r *RAG) ListFiles(ctx context.Context) ([]FileStat, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT rel_path, COUNT(*) as chunks, MAX(created_at) as last
		FROM chunks
		GROUP BY rel_path
		ORDER BY last DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("rag: list files: %w", err)
	}
	defer rows.Close()

	var out []FileStat
	for rows.Next() {
		var fs FileStat
		var last int64
		if err := rows.Scan(&fs.RelPath, &fs.Chunks, &last); err != nil {
			return nil, fmt.Errorf("rag: scan file stat: %w", err)
		}
		out = append(out, fs)
	}
	return out, rows.Err()
}

type doclingOutput struct {
	OK       bool     `json:"ok"`
	Markdown string   `json:"markdown"`
	Error    string   `json:"error"`
	Partial  bool     `json:"partial"`
	Errors   []string `json:"errors"`
}

func parseDoclingOutput(raw []byte) (string, error) {
	var out doclingOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("docling: could not parse output as JSON: %w (raw: %s)", err, string(raw))
	}
	if !out.OK {
		return "", fmt.Errorf("docling: %s", out.Error)
	}
	return out.Markdown, nil
}
