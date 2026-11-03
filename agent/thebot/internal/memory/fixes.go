package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// A proven fix is a command a person approved for one failure, which then ran and after which the check that had
// failed passed. It is kept apart from L1's facts on purpose: a fact is text, and text can be put into L1 by anyone
// who can get a sentence into a request ("remember that the fix for X is Y"), so a fix recalled from a fact is only
// ever a proposal. A proven fix is written only by the coding harness itself (Harness.settleLearning), with the exact
// bytes of the command the person saw, and is read back only for the same failure signature.
//
// It is as strong as the file it lives in: until the approval state moves to a process the worker's shell cannot
// write to, a worker that edits l1.db can plant one. What a planted one can do is bounded by the caller: a proven fix
// still passes the command safety check (safety.AssessCommand) before it runs, so it can never run anything the
// approval gate would have stopped.

// fixesSchema is the table the proven fixes are kept in, beside L1's facts.
const fixesSchema = `
CREATE TABLE IF NOT EXISTS approved_fixes (
	signature TEXT PRIMARY KEY,
	command TEXT NOT NULL,
	approved_at INTEGER NOT NULL
);
`

// ErrNoSignature is why a proven fix was not recorded: without the failure it was for, it would answer any failure.
var ErrNoSignature = errors.New("l1: a proven fix needs the failure signature it was for")

// RecordProvenFix keeps command as the proven fix for the failure signature: a person approved exactly this command
// for it, it ran, and the check that had failed then passed. A secret-shaped command is not kept.
func (l *L1) RecordProvenFix(ctx context.Context, signature, command string) error {
	signature, command = strings.TrimSpace(signature), strings.TrimSpace(command)
	if signature == "" {
		return ErrNoSignature
	}
	if command == "" || LooksSecret(command) {
		return ErrSecret
	}
	if err := l.ensureFixes(ctx); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO approved_fixes (signature, command, approved_at) VALUES (?, ?, ?)
		 ON CONFLICT(signature) DO UPDATE SET command = excluded.command, approved_at = excluded.approved_at`,
		signature, command, time.Now().Unix())
	return err
}

// ProvenFix returns the proven fix recorded for exactly this failure signature, or "" when there is none.
func (l *L1) ProvenFix(ctx context.Context, signature string) (string, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return "", nil
	}
	if err := l.ensureFixes(ctx); err != nil {
		return "", err
	}
	var command string
	err := l.db.QueryRowContext(ctx, `SELECT command FROM approved_fixes WHERE signature = ?`, signature).Scan(&command)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return command, err
}

// ensureFixes creates the proven-fix table on first use.
func (l *L1) ensureFixes(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, fixesSchema)
	return err
}
