package catalog

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

type backend string

const (
	backendSQLite   backend = "sqlite"
	backendPostgres backend = "postgres"
)

// catalogDB keeps driver-specific placeholder handling out of catalog logic.
// The catalog deliberately uses only database/sql so SQLite and PostgreSQL
// share transactions, cancellation, and connection-pool behavior.
type catalogDB struct {
	raw     *sql.DB
	backend backend
}

type catalogTx struct {
	raw     *sql.Tx
	backend backend
}

func (d *catalogDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.raw.ExecContext(ctx, d.bind(query), args...)
}

func (d *catalogDB) Exec(query string, args ...any) (sql.Result, error) {
	return d.raw.Exec(d.bind(query), args...)
}

func (d *catalogDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.raw.QueryContext(ctx, d.bind(query), args...)
}

func (d *catalogDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.raw.QueryRowContext(ctx, d.bind(query), args...)
}

func (d *catalogDB) BeginTx(ctx context.Context, options *sql.TxOptions) (*catalogTx, error) {
	tx, err := d.raw.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &catalogTx{raw: tx, backend: d.backend}, nil
}

func (d *catalogDB) Close() error { return d.raw.Close() }

func (d *catalogDB) bind(query string) string {
	if d.backend != backendPostgres {
		return query
	}
	return postgresPlaceholders(query)
}

func (t *catalogTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.raw.ExecContext(ctx, bindQuery(t.backend, query), args...)
}

func (t *catalogTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.raw.QueryContext(ctx, bindQuery(t.backend, query), args...)
}

func (t *catalogTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.raw.QueryRowContext(ctx, bindQuery(t.backend, query), args...)
}

func (t *catalogTx) Commit() error   { return t.raw.Commit() }
func (t *catalogTx) Rollback() error { return t.raw.Rollback() }

func bindQuery(kind backend, query string) string {
	if kind != backendPostgres {
		return query
	}
	return postgresPlaceholders(query)
}

// postgresPlaceholders replaces positional question marks outside SQL string
// literals. Catalog SQL does not use driver placeholders inside literals, but
// handling quoted text avoids corrupting LIKE escapes and search expressions.
func postgresPlaceholders(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 16)
	inString := false
	arg := 1
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			out.WriteByte(ch)
			if inString && i+1 < len(query) && query[i+1] == '\'' {
				out.WriteByte(query[i+1])
				i++
				continue
			}
			inString = !inString
			continue
		}
		if !inString && strings.HasPrefix(query[i:], "is_folder = 0") {
			out.WriteString("is_folder = FALSE")
			i += len("is_folder = 0") - 1
			continue
		}
		if !inString && strings.HasPrefix(query[i:], "is_folder = 1") {
			out.WriteString("is_folder = TRUE")
			i += len("is_folder = 1") - 1
			continue
		}
		if ch == '?' && !inString {
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(arg))
			arg++
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}
