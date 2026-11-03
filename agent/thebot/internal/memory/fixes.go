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
// It is as strong as the file it lives in - the worker's shell can write l1.db - so it is never a yes (review T-08,
// CLAUDE.md "no always allow"): the same failure is asked about again, and the record only lets the question show the
// person their earlier answer ("you approved this on ..."). Inside Decurix none is kept or read at all
// (coding.Harness.NoProvenFixes; the coding harness is off there too).
//
// A yes is not an "always allow" (round-3 recheck): a proven fix stands only in the project it was approved in (its
// scope, the build's folder) and only for ProvenFixMaxAge; after that, or anywhere else, the same failure asks again.
// The first table kept fixes by failure alone, so a fix approved in one project ran without asking in every other,
// forever; it is no longer read.

// ProvenFixMaxAge is how long a person's yes to a fix stands for the same failure in the same project.
const ProvenFixMaxAge = 30 * 24 * time.Hour

// fixesSchema is the table the proven fixes are kept in, beside L1's facts.
const fixesSchema = `
CREATE TABLE IF NOT EXISTS approved_fixes_scoped (
	signature TEXT NOT NULL,
	scope TEXT NOT NULL,
	command TEXT NOT NULL,
	approved_at INTEGER NOT NULL,
	PRIMARY KEY (signature, scope)
);
`

// ErrNoSignature is why a proven fix was not recorded: without the failure it was for, it would answer any failure.
var ErrNoSignature = errors.New("l1: a proven fix needs the failure signature it was for")

// ErrNoScope is why a proven fix was not recorded: without the project it was approved in, it would stand in all.
var ErrNoScope = errors.New("l1: a proven fix needs the project it was approved in")

// RecordProvenFix keeps command as the proven fix for the failure signature in scope (the project): a person approved
// exactly this command for it there, it ran, and the check that had failed then passed. A secret-shaped command is not
// kept.
func (l *L1) RecordProvenFix(ctx context.Context, signature, scope, command string) error {
	signature, scope, command = strings.TrimSpace(signature), strings.TrimSpace(scope), strings.TrimSpace(command)
	if signature == "" {
		return ErrNoSignature
	}
	if scope == "" {
		return ErrNoScope
	}
	if command == "" || LooksSecret(command) {
		return ErrSecret
	}
	if err := l.ensureFixes(ctx); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO approved_fixes_scoped (signature, scope, command, approved_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(signature, scope) DO UPDATE SET command = excluded.command, approved_at = excluded.approved_at`,
		signature, scope, command, time.Now().Unix())
	return err
}

// ProvenFix returns the proven fix recorded for exactly this failure signature in scope, approved within
// ProvenFixMaxAge, or "" when there is none.
func (l *L1) ProvenFix(ctx context.Context, signature, scope string) (string, error) {
	command, _, err := l.ProvenFixAt(ctx, signature, scope)
	return command, err
}

// ProvenFixAt is ProvenFix with when the person approved it: what a new question about the same failure shows them
// ("you approved this on ..."). It never stands for a yes (review T-08: no remembered yes).
func (l *L1) ProvenFixAt(ctx context.Context, signature, scope string) (string, time.Time, error) {
	signature, scope = strings.TrimSpace(signature), strings.TrimSpace(scope)
	if signature == "" || scope == "" {
		return "", time.Time{}, nil
	}
	if err := l.ensureFixes(ctx); err != nil {
		return "", time.Time{}, err
	}
	var command string
	var at int64
	err := l.db.QueryRowContext(ctx, `SELECT command, approved_at FROM approved_fixes_scoped WHERE signature = ? AND scope = ?`,
		signature, scope).Scan(&command, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if age := time.Since(time.Unix(at, 0)); age > ProvenFixMaxAge || age < -time.Hour {
		return "", time.Time{}, nil
	}
	return command, time.Unix(at, 0), nil
}

// AgeProvenFix moves a proven fix's approval back by d (for tests of its expiry).
func (l *L1) AgeProvenFix(ctx context.Context, signature, scope string, d time.Duration) error {
	if err := l.ensureFixes(ctx); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx, `UPDATE approved_fixes_scoped SET approved_at = approved_at - ? WHERE signature = ? AND scope = ?`,
		int64(d.Seconds()), signature, scope)
	return err
}

// ensureFixes creates the proven-fix table on first use.
func (l *L1) ensureFixes(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, fixesSchema)
	return err
}
