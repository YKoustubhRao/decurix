// purge.go holds the two helpers a purge of a leaked secret (internal/purge) uses in every store that keeps text:
// rewriting matching rows of a table in one statement, and replacing the same forms in a Go string. Neither knows
// what a secret is; the caller hands them the forms the value may be stored in and the mark that replaces them.

package sqlitedb

import (
	"database/sql"
	"strings"
)

// Execer is a *sql.DB, *sql.Conn or *sql.Tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// ReplaceInColumns rewrites, in one statement, every row of table whose columns hold any of forms, with each form
// replaced by mark (forms in the order given: longest first, so a form inside a longer one is not replaced first). It
// returns how many rows changed. SQLite does the scan, so no row is loaded here. table and columns are the caller's
// constants, never input.
func ReplaceInColumns(db Execer, table string, columns, forms []string, mark string) (int64, error) {
	forms = nonEmpty(forms)
	if len(columns) == 0 || len(forms) == 0 {
		return 0, nil
	}
	var set, where []string
	var setArgs, whereArgs []any
	for _, c := range columns {
		expr := c
		for _, f := range forms {
			expr = "REPLACE(" + expr + ", ?, ?)"
			setArgs = append(setArgs, f, mark)
			where = append(where, "instr("+c+", ?) > 0")
			whereArgs = append(whereArgs, f)
		}
		set = append(set, c+" = "+expr)
	}
	res, err := db.Exec(`UPDATE `+table+` SET `+strings.Join(set, ", ")+` WHERE `+strings.Join(where, " OR "),
		append(setArgs, whereArgs...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeText is s with each of forms replaced by mark, in the order given, and whether anything was replaced.
func PurgeText(s string, forms []string, mark string) (string, bool) {
	changed := false
	for _, f := range forms {
		if f != "" && strings.Contains(s, f) {
			s = strings.ReplaceAll(s, f, mark)
			changed = true
		}
	}
	return s, changed
}

// nonEmpty is forms without the empty ones (REPLACE with an empty pattern is a no-op; instr of one always matches).
func nonEmpty(forms []string) []string {
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
