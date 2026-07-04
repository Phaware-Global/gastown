package reaper

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestIsMissingTableErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"typed ER_NO_SUCH_TABLE", &mysql.MySQLError{Number: erNoSuchTable, Message: "table not found: wisps"}, true},
		{"wrapped typed ER_NO_SUCH_TABLE", fmt.Errorf("show columns: %w", &mysql.MySQLError{Number: erNoSuchTable, Message: "table not found: wisps"}), true},
		// Typed errors are classified strictly by number: a message that
		// happens to contain "doesn't exist" must not read as table-absent.
		{"typed unknown database", &mysql.MySQLError{Number: 1049, Message: "database hq doesn't exist"}, false},
		{"typed access denied", &mysql.MySQLError{Number: 1045, Message: "Access denied for user"}, false},
		// Untyped errors fall back to the substring check.
		{"untyped table not found", errors.New("table not found: wisps"), true},
		{"untyped doesn't exist", errors.New("Table 'hq.wisps' doesn't exist"), true},
		{"untyped connectivity", errors.New("dial tcp 127.0.0.1:3307: connection refused"), false},
	}
	for _, tt := range tests {
		if got := isMissingTableErr(tt.err); got != tt.want {
			t.Errorf("%s: isMissingTableErr() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

var typedDepColumns = []string{"id", "Depends_On_Issue_Id", "DEPENDS_ON_WISP_ID", "depends_on_external"}

func TestHasReaperSchemaViaShowColumns(t *testing.T) {
	fullSchema := map[string][]string{
		"wisps":             {"ID", "Status", "closed_at"},
		"issues":            {"id", "status"},
		"wisp_dependencies": typedDepColumns,
		"dependencies":      typedDepColumns,
	}
	withoutTable := func(name string) map[string][]string {
		tables := make(map[string][]string, len(fullSchema))
		for k, v := range fullSchema {
			if k != name {
				tables[k] = v
			}
		}
		return tables
	}

	tests := []struct {
		name    string
		tables  map[string][]string
		errs    map[string]error
		want    bool
		wantErr bool
	}{
		// Column matching must be case-insensitive, like the
		// information_schema queries SHOW COLUMNS replaced.
		{name: "full schema, mixed-case columns", tables: fullSchema, want: true},
		{name: "missing wisps table", tables: withoutTable("wisps"), want: false},
		{name: "missing issues table", tables: withoutTable("issues"), want: false},
		{name: "missing wisp_dependencies table", tables: withoutTable("wisp_dependencies"), want: false},
		// The dependencies table is optional; absent means schema OK.
		{name: "dependencies table optional", tables: withoutTable("dependencies"), want: true},
		{
			name: "wisp_dependencies lacks typed columns",
			tables: map[string][]string{
				"wisps":             {"id"},
				"issues":            {"id"},
				"wisp_dependencies": {"id", "depends_on_id"},
				"dependencies":      typedDepColumns,
			},
			want: false,
		},
		{
			name: "dependencies present but lacks typed columns",
			tables: map[string][]string{
				"wisps":             {"id"},
				"issues":            {"id"},
				"wisp_dependencies": typedDepColumns,
				"dependencies":      {"id", "depends_on_id"},
			},
			want: false,
		},
		// Real errors must surface, not read as "no schema" (hq-09sb1).
		{
			name:    "connectivity error surfaces",
			tables:  fullSchema,
			errs:    map[string]error{"wisps": errors.New("dial tcp 127.0.0.1:3307: connection refused")},
			want:    false,
			wantErr: true,
		},
		{
			name:    "typed permission error surfaces",
			tables:  fullSchema,
			errs:    map[string]error{"wisps": &mysql.MySQLError{Number: 1045, Message: "Access denied for user"}},
			want:    false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openFakeSchemaDB(t, &fakeSchemaState{tables: tt.tables, errs: tt.errs})
			defer db.Close()

			got, err := HasReaperSchema(db)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HasReaperSchema() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("HasReaperSchema() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- minimal fake driver serving only SHOW COLUMNS FROM `<table>` ---

var fakeSchemaDriverID uint64

func openFakeSchemaDB(t *testing.T, state *fakeSchemaState) *sql.DB {
	t.Helper()
	driverName := fmt.Sprintf("fake_schema_%d", atomic.AddUint64(&fakeSchemaDriverID, 1))
	sql.Register(driverName, &fakeSchemaDriver{state: state})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	return db
}

type fakeSchemaState struct {
	tables map[string][]string // table -> column names (schema-defined case)
	errs   map[string]error    // table -> error injected for its SHOW COLUMNS
}

type fakeSchemaDriver struct{ state *fakeSchemaState }

func (d *fakeSchemaDriver) Open(string) (driver.Conn, error) {
	return &fakeSchemaConn{state: d.state}, nil
}

type fakeSchemaConn struct{ state *fakeSchemaState }

func (c *fakeSchemaConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakeSchemaConn: Prepare unsupported")
}
func (c *fakeSchemaConn) Close() error              { return nil }
func (c *fakeSchemaConn) Begin() (driver.Tx, error) { return nil, errors.New("unsupported") }

func (c *fakeSchemaConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	const prefix = "SHOW COLUMNS FROM `"
	if !strings.HasPrefix(query, prefix) || !strings.HasSuffix(query, "`") {
		return nil, fmt.Errorf("fakeSchemaConn: unexpected query %q", query)
	}
	table := strings.TrimSuffix(strings.TrimPrefix(query, prefix), "`")
	if err := c.state.errs[table]; err != nil {
		return nil, err
	}
	cols, ok := c.state.tables[table]
	if !ok {
		return nil, &mysql.MySQLError{Number: erNoSuchTable, Message: "table not found: " + table}
	}
	return &fakeSchemaRows{cols: cols}, nil
}

type fakeSchemaRows struct {
	cols []string
	i    int
}

func (r *fakeSchemaRows) Columns() []string {
	return []string{"Field", "Type", "Null", "Key", "Default", "Extra"}
}
func (r *fakeSchemaRows) Close() error { return nil }
func (r *fakeSchemaRows) Next(dest []driver.Value) error {
	if r.i >= len(r.cols) {
		return io.EOF
	}
	dest[0] = r.cols[r.i]
	for i := 1; i < len(dest); i++ {
		dest[i] = ""
	}
	r.i++
	return nil
}
