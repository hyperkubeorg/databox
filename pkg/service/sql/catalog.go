// catalog.go defines the on-cluster schema catalog and the key encodings
// that map SQL tables and indexes onto databox key ranges
// (§13). Because the engine owns the key layout, grants
// apply at table granularity through ordinary prefix rules (§7.2).
//
// Key layout for database <db>:
//
//	/sql/<db>/_catalog/<table>            → tableSchema (JSON)
//	/sql/<db>/<table>/<pkhex>             → stored row (see value.go encodeRow)
//	/sql/<db>/_idx/<table>/<index>/<keyhex>/<pkhex> → the row's primary key
//
// <pkhex> and <keyhex> are the order-preserving hex encodings from
// keyenc.go, so a table or index scan is a single ranged List that already
// returns rows in key order.
package sql

import (
	"encoding/json"
	"strings"
)

// column is one stored column definition.
type column struct {
	Name     string `json:"name"`
	Type     Type   `json:"type"`
	TypeName string `json:"type_name"`
	// Dim is the declared VECTOR(n) dimension, enforced on every write;
	// 0 for non-vector columns.
	Dim     int  `json:"dim,omitempty"`
	NotNull bool `json:"not_null,omitempty"`
	Unique  bool `json:"unique,omitempty"`
	// Auto marks the auto-increment primary-key column (SERIAL /
	// AUTO_INCREMENT): a NULL/omitted insert value draws from the table's
	// NextRowID counter.
	Auto bool `json:"auto,omitempty"`
	// Default holds the SQL text of a DEFAULT clause; empty when absent.
	Default string `json:"default,omitempty"`
}

// indexDef is one secondary index (also used to represent a table-level
// UNIQUE constraint, which is a unique index under the hood).
type indexDef struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
}

// checkDef is one CHECK constraint: its chai-style name (<table>_check,
// <table>_check1, ...) and the SQL text of its expression, re-parsed at
// enforcement time.
type checkDef struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// tableSchema is the catalog record for one table.
type tableSchema struct {
	Name    string     `json:"name"`
	Columns []column   `json:"columns"`
	PK      []string   `json:"pk,omitempty"` // primary-key column names; empty = implicit rowid
	Indexes []indexDef `json:"indexes,omitempty"`
	Checks  []checkDef `json:"checks,omitempty"` // CHECK constraints, enforced on write
	// NextRowID feeds the implicit rowid for tables without a declared
	// primary key. It is bumped inside the same transaction as the insert,
	// so it never races.
	NextRowID int64 `json:"next_rowid,omitempty"`
}

// col returns the named column definition, or false.
func (t *tableSchema) col(name string) (column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return column{}, false
}

// hasPK reports whether the table declared a primary key (vs. implicit
// rowid).
func (t *tableSchema) hasPK() bool { return len(t.PK) > 0 }

// autoCol returns the auto-increment column's name, or "" when the table
// has none. At most one exists (it must be the single-column primary key).
func (t *tableSchema) autoCol() string {
	for _, c := range t.Columns {
		if c.Auto {
			return c.Name
		}
	}
	return ""
}

func (t *tableSchema) encode() []byte { b, _ := json.Marshal(t); return b }

func decodeSchema(b []byte) (*tableSchema, error) {
	var t tableSchema
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// --- key builders ------------------------------------------------------------

// sqlRoot is the fixed root prefix for all SQL data (configurable root is
// future work; the constant keeps grants and backups predictable).
const sqlRoot = "/sql/"

func catalogKey(db, table string) string    { return sqlRoot + db + "/_catalog/" + table }
func catalogPrefix(db string) string        { return sqlRoot + db + "/_catalog/" }
func tablePrefix(db, table string) string   { return sqlRoot + db + "/" + table + "/" }
func rowKey(db, table, pkhex string) string { return tablePrefix(db, table) + pkhex }
func indexPrefix(db, table string) string   { return sqlRoot + db + "/_idx/" + table + "/" }
func oneIndexPrefix(db, table, idx string) string {
	return indexPrefix(db, table) + idx + "/"
}

// indexEntryKey is the key for one index entry: the encoded index columns
// followed by the encoded primary key, so multiple rows sharing an index
// value stay distinct and the row's PK is recoverable from the key.
func indexEntryKey(db, table, idx, keyhex, pkhex string) string {
	return oneIndexPrefix(db, table, idx) + keyhex + "/" + pkhex
}

// tableFromCatalogKey extracts the table name from a catalog key for
// listing all tables in a database.
func tableFromCatalogKey(key, db string) string {
	return strings.TrimPrefix(key, catalogPrefix(db))
}
