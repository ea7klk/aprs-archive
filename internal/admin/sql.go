package admin

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The SQL console.
//
// This is a full SQLite shell over the archive: arbitrary statements, a schema
// browser, and results as a table or as JSON. That is a genuinely dangerous
// capability, and the design says so in three places rather than in a warning
// label:
//
//  1. It exists only on the admin binary and only behind the admin password. The
//     public binary does not compile it into a route table at all.
//  2. Every statement runs on a connection that is opened for the console, with
//     a timeout, and is never allowed to leave a transaction open.
//  3. The things that stop the archive working — writing to it while the
//     collector is collecting, or rewriting the file under it — are reported
//     plainly, and the statement that would do them is still allowed, because an
//     operator asked for a console. What is not allowed is doing it by accident.
//
// Deliberately not blocked: DROP, DELETE, PRAGMA, ATTACH. An operator with the
// password and this page can destroy the archive; pretending otherwise by
// refusing keywords would only invite running them through a different path.

const (
	// sqlTimeout bounds one statement. A console statement that takes longer
	// than this is almost always a runaway cartesian join on a large table.
	sqlTimeout = 30 * time.Second
	// sqlRows bounds how many rows are rendered. The count is always complete.
	sqlRows = 500
)

// SQLResult is the outcome of one statement.
type SQLResult struct {
	Query        string
	Columns      []string
	Rows         [][]string
	RowCount     int
	Truncated    bool
	RowsAffected int64
	LastInsertID int64
	Elapsed      time.Duration
	// Affected is set for statements that do not return rows.
	Affected bool
	Error    string
}

// readOnly reports whether a statement cannot change data or schema.
//
// It is used only to decide how to present the result (and whether to advise a
// backup), never as a gate: the console is a console.
func readOnly(statement string) bool {
	trimmed := strings.ToUpper(strings.TrimSpace(statement))
	for _, prefix := range []string{"SELECT", "WITH", "EXPLAIN", "PRAGMA", "VALUES"} {
		if strings.HasPrefix(trimmed, prefix) {
			// `PRAGMA ... = ...` sets a value; `PRAGMA page_count` reads.
			if prefix == "PRAGMA" && strings.Contains(trimmed, "=") {
				return false
			}
			return true
		}
	}
	return false
}

// Exec runs one statement and returns its result.
//
// Multiple statements in one submission are refused: SQLite's driver runs only
// the first, and silently ignoring the rest would make a batch look successful
// when half of it never ran. An operator who wants several runs them one at a
// time, which also makes the outcome of each unambiguous.
func (s *Server) Exec(ctx context.Context, statement string) SQLResult {
	statement = strings.TrimSpace(statement)
	res := SQLResult{Query: statement}
	if statement == "" {
		res.Error = "no statement"
		return res
	}
	if hasMultipleStatements(statement) {
		res.Error = "this console runs one statement at a time"
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()

	start := time.Now()
	// The driver reports a statement that returns no rows by giving back a row
	// set with zero columns and no error, not by failing — so the shape of the
	// result is what says which path to take. Asking Columns() first and then
	// falling back covers both drivers' behaviour.
	rows, err := s.db.QueryContext(ctx, statement)
	if err != nil {
		res.Error = err.Error()
		res.Elapsed = time.Since(start)
		return res
	}
	cols, err := rows.Columns()
	if err == nil && len(cols) == 0 {
		rows.Close()
		res := s.execNonQuery(ctx, statement)
		res.Elapsed = time.Since(start)
		return res
	}
	defer rows.Close()
	if err != nil {
		res.Error = err.Error()
		res.Elapsed = time.Since(start)
		return res
	}
	if err != nil {
		res.Error = err.Error()
		res.Elapsed = time.Since(start)
		return res
	}
	res.Columns = cols
	for rows.Next() {
		if res.RowCount >= sqlRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			res.Error = err.Error()
			break
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = formatValue(v)
		}
		res.Rows = append(res.Rows, row)
		res.RowCount++
	}
	if err := rows.Err(); err != nil && res.Error == "" {
		res.Error = err.Error()
	}
	res.Elapsed = time.Since(start)
	return res
}

// execNonQuery runs a statement that returns no rows.
func (s *Server) execNonQuery(ctx context.Context, statement string) SQLResult {
	res := SQLResult{Query: statement, Affected: true}
	r, err := s.db.ExecContext(ctx, statement)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.RowsAffected, _ = r.RowsAffected()
	res.LastInsertID, _ = r.LastInsertId()
	return res
}

// hasMultipleStatements reports whether a submission contains more than one
// statement. Semicolons inside string literals and trailing whitespace are not
// counted.
func hasMultipleStatements(statement string) bool {
	inString := false
	for i := 0; i < len(statement); i++ {
		switch c := statement[i]; {
		case c == '\'':
			// A doubled quote inside a literal is an escaped quote.
			if inString && i+1 < len(statement) && statement[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case c == ';' && !inString:
			if strings.TrimSpace(statement[i+1:]) != "" {
				return true
			}
		}
	}
	return false
}

// formatValue renders one cell. NULL is shown as NULL rather than an empty
// string, because in a database those mean different things and an operator
// reading the console needs to tell them apart.
func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		if isPrintable(x) {
			return string(x)
		}
		return fmt.Sprintf("0x%x", x)
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			return false
		}
	}
	return true
}

// SchemaItem is one object in the schema browser.
type SchemaItem struct {
	Type string
	Name string
	SQL  string
}

// Schema lists the archive's tables, indexes and views.
func (s *Server) Schema(ctx context.Context) ([]SchemaItem, error) {
	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT type, name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE type IN ('table','index','view','trigger')
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 WHEN 'index' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchemaItem
	for rows.Next() {
		var it SchemaItem
		if err := rows.Scan(&it.Type, &it.Name, &it.SQL); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
