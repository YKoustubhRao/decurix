package bench

import (
	"context"
	"regexp"
	"strings"

	"brainiac/internal/web"
)

var (
	tableRe   = regexp.MustCompile(`(?is)<table\b.*?</table>`)
	trRe      = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	cellRe    = regexp.MustCompile(`(?is)<t([hd])\b([^>]*)>(.*?)</t[hd]>`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	firstNum  = regexp.MustCompile(`-?\d+(?:\.\d+)?%?`)
	mdSepRe   = regexp.MustCompile(`^\|?\s*:?-{2,}`)
	spaceRe   = regexp.MustCompile(`\s+`)
	colspanRe = regexp.MustCompile(`(?i)colspan\s*=\s*["']?(\d+)`)
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// CardScores reads the comparison tables of a model card: each column is a
// model, each row a benchmark. They are the vendor's own runs, so they count
// as one source (the card), never as independent.
func CardScores(ctx context.Context, w *web.Client, repo string) ([]Score, error) {
	body, err := w.FetchRaw(ctx, "https://huggingface.co/"+repo+"/raw/main/README.md")
	if err != nil {
		return nil, err
	}
	return ParseCard(string(body), "card "+repo), nil
}

// ParseCard reads every HTML and Markdown table in a card.
func ParseCard(text, source string) []Score {
	var out []Score
	for _, t := range tableRe.FindAllString(text, -1) {
		out = append(out, tableScores(htmlRows(t), source)...)
	}
	for _, t := range markdownTables(tableRe.ReplaceAllString(text, "")) {
		out = append(out, tableScores(t, source)...)
	}
	return out
}

func cellText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "<br>", " "), "<br/>", " ")
	s = mdLinkRe.ReplaceAllString(mdImageRe.ReplaceAllString(s, ""), "$1")
	return strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(s, " "), " "))
}

// htmlRows is a table's rows as cell texts; a row that is one cell spanning
// the table (a section heading) is left out.
func htmlRows(table string) [][]string {
	var rows [][]string
	for _, tr := range trRe.FindAllStringSubmatch(table, -1) {
		cells := cellRe.FindAllStringSubmatch(tr[1], -1)
		if len(cells) == 1 {
			if m := colspanRe.FindStringSubmatch(cells[0][2]); m != nil {
				continue
			}
		}
		var row []string
		for _, c := range cells {
			row = append(row, cellText(c[3]))
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

// markdownTables splits Markdown text into its pipe tables.
func markdownTables(text string) [][][]string {
	var tables [][][]string
	var cur [][]string
	flush := func() {
		if len(cur) >= 2 {
			tables = append(tables, cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "|") {
			flush()
			continue
		}
		if mdSepRe.MatchString(l) {
			continue
		}
		parts := strings.Split(strings.Trim(l, "|"), "|")
		row := make([]string, len(parts))
		for i, p := range parts {
			row[i] = cellText(strings.ReplaceAll(p, "**", ""))
		}
		cur = append(cur, row)
	}
	flush()
	return tables
}

// tableScores turns a table whose first row names models into scores. The
// first column names the benchmark; rows for benchmarks no role uses, and
// cells that are not numbers, are skipped. A table with models in its rows
// (its header names benchmarks) is turned first.
func tableScores(rows [][]string, source string) []Score {
	if len(rows) < 2 {
		return nil
	}
	if benchmarkCount(rows[0][1:]) > benchmarkCount(firstColumn(rows[1:])) {
		rows = transpose(rows)
	}
	if len(rows) < 2 || len(rows[0]) < 3 {
		return nil
	}
	head := rows[0]
	var out []Score
	for _, r := range rows[1:] {
		if len(r) < 2 {
			continue
		}
		bench := strings.TrimSpace(r[0])
		if RoleOf(bench) == "" {
			continue
		}
		// A table whose header has one fewer cell than its rows has an empty
		// corner cell left out.
		offset := 0
		if len(r) == len(head)+1 {
			offset = 1
		}
		for i := 1; i < len(r); i++ {
			hi := i - offset
			if hi <= 0 || hi >= len(head) {
				continue
			}
			model := Key(head[hi])
			if model == "" {
				continue
			}
			m := firstNum.FindString(r[i])
			v, ok := num(m, false)
			if !ok {
				continue
			}
			out = append(out, Score{Model: model, Raw: head[hi], Benchmark: strings.ToLower(bench), Value: v,
				Lower: lowerBetter(bench), Source: source})
		}
	}
	return out
}

func benchmarkCount(cells []string) int {
	n := 0
	for _, c := range cells {
		if RoleOf(c) != "" {
			n++
		}
	}
	return n
}

func firstColumn(rows [][]string) []string {
	var out []string
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r[0])
		}
	}
	return out
}

// transpose turns rows into columns, padding short rows.
func transpose(rows [][]string) [][]string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	out := make([][]string, width)
	for i := range out {
		out[i] = make([]string, len(rows))
		for j, r := range rows {
			if i < len(r) {
				out[i][j] = r[i]
			}
		}
	}
	return out
}
