// Package testfixture contains the SQLite-over-HTTP confinement regression
// fixture. Production Test workers never import or start this database.
package testfixture

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	_ "modernc.org/sqlite"
)

const DatabaseURL = "http://127.0.0.1:17681/transaction"

// Statement is one parameterized SQLite statement. A request is one atomic
// transaction; this is a deliberately custom HTTP protocol, not PostgreSQL/MySQL.
type Statement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
}
type QueryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// DatabaseHandler serves real transactions against a fresh private database.
// The caller owns the listener and MUST bind it inside the network namespace.
func DatabaseHandler(path string) (http.Handler, func() error, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, nil, err
	}
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, nil, err
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/transaction" || r.URL.RawQuery != "" {
			http.Error(w, "only POST /transaction is supported", 404)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var statements []Statement
		if err := decoder.Decode(&statements); err != nil || len(statements) == 0 || len(statements) > 128 {
			http.Error(w, "expected 1..128 statements", 400)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "unexpected trailing input", 400)
			return
		}
		tx, err := d.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, "begin transaction failed", 500)
			return
		}
		defer tx.Rollback()
		results := make([]QueryResult, 0, len(statements))
		for _, statement := range statements {
			rows, err := tx.QueryContext(r.Context(), statement.SQL, statement.Args...)
			if err != nil {
				http.Error(w, fmt.Sprintf("SQLite: %v", err), 400)
				return
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				http.Error(w, "read columns failed", 500)
				return
			}
			result := QueryResult{Columns: columns, Rows: [][]any{}}
			size := 0
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					rows.Close()
					http.Error(w, "scan failed", 500)
					return
				}
				for i, v := range values {
					if b, ok := v.([]byte); ok {
						values[i] = string(b)
					}
				}
				encoded, _ := json.Marshal(values)
				size += len(encoded)
				if len(result.Rows) >= 10000 || size > 1<<20 {
					rows.Close()
					http.Error(w, "result exceeds limit", 413)
					return
				}
				result.Rows = append(result.Rows, values)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				http.Error(w, "query failed", 400)
				return
			}
			results = append(results, result)
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit failed", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})
	return h, d.Close, nil
}
