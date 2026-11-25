package database

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Only the original Text/Integer declarations are migrated. Extension columns
// and already upgraded declarations retain their original SQL and storage.
type historicalTypes struct {
	table, from, to string
	fields          []string
}

var historicalTypeChanges = []historicalTypes{
	{"CUSTOM_WORDS", "INTEGER", "TEXT", []string{"OFFSET"}},
	{"SITE_BRUSH_TASK", "TEXT", "INTEGER", []string{"DOWNLOAD_COUNT", "REMOVE_COUNT", "DOWNLOAD_SIZE", "UPLOAD_SIZE"}},
}

func quoted(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func (change historicalTypes) apply(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT name, upper(type), hidden, pk FROM pragma_table_xinfo(?) ORDER BY cid", change.table)
	if err != nil {
		return err
	}
	var writeCols, selectCols, keys, checks []string
	used, changed := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var name, typ string
		var hidden, primaryKey int
		if err := rows.Scan(&name, &typ, &hidden, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		key := strings.ToUpper(name)
		used[key] = true
		if hidden != 0 { // SQLite recomputes generated columns.
			continue
		}
		col := quoted(name)
		writeCols = append(writeCols, col)
		expr := "o." + col
		if slices.Contains(change.fields, key) && typ == change.from {
			changed[key] = true
			expr = "CAST(o." + col + " AS " + change.to + ")"
		}
		selectCols = append(selectCols, expr)
		checks = append(checks, "n."+col+" COLLATE BINARY IS "+expr+" COLLATE BINARY AND typeof(n."+col+")=typeof("+expr+")")
		if primaryKey > 0 {
			keys = append(keys, "n."+col+" IS "+expr)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(changed) == 0 {
		return err
	}
	var ddl, originalName string
	var withoutRowID bool
	if err := tx.QueryRow(`SELECT s.sql, s.name, p.wr FROM sqlite_schema s JOIN pragma_table_list p
		ON p.name=s.name AND p.schema='main' AND p.type='table' WHERE s.type='table' AND s.name=? COLLATE NOCASE`, change.table).Scan(&ddl, &originalName, &withoutRowID); err != nil {
		return err
	}
	if !withoutRowID {
		var rowID string
		for _, alias := range []string{"_ROWID_", "ROWID", "OID"} {
			if !used[alias] {
				rowID = alias
				break
			}
		}
		if rowID == "" {
			return errors.New("all SQLite rowid names are shadowed")
		}
		writeCols = append(writeCols, rowID)
		selectCols = append(selectCols, "o."+rowID)
		keys = []string{"n." + rowID + "=o." + rowID}
	}
	for _, name := range change.fields {
		if !changed[name] {
			continue
		}
		if err := change.validateValues(tx, name); err != nil {
			return err
		}
	}
	shadow := "nastool_type_" + rand.Text()
	create, err := change.revisedDDL(ddl, shadow, changed)
	if err != nil {
		return err
	}
	objects, err := schemaObjects(tx, originalName)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(create); err != nil {
		return err
	}
	original := quoted(originalName)
	if _, err := tx.Exec("INSERT OR ABORT INTO " + quoted(shadow) + " (" + strings.Join(writeCols, ",") + ") SELECT " + strings.Join(selectCols, ",") + " FROM " + original + " AS o"); err != nil {
		return err
	}
	// Extensions can place constraints on the converted columns. In particular
	// an INTEGER PRIMARY KEY can become an alias for rowid. Verify identities
	// and every copied value before the original table can be dropped.
	var differs bool
	verifyRows := "SELECT EXISTS(SELECT 1 FROM " + original + " AS o WHERE NOT EXISTS(SELECT 1 FROM " + quoted(shadow) + " AS n WHERE " + strings.Join(keys, " AND ") + " AND " + strings.Join(checks, " AND ") + "))"
	if err := tx.QueryRow(verifyRows).Scan(&differs); err != nil {
		return err
	}
	if differs {
		return errors.New("table constraints changed a copied row or its identity")
	}
	// A deleted high ID must never be issued again by an AUTOINCREMENT table.
	var hasSequence bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='sqlite_sequence')").Scan(&hasSequence); err != nil {
		return err
	}
	var seq sql.NullInt64
	if hasSequence {
		if err := tx.QueryRow("SELECT max(seq) FROM sqlite_sequence WHERE name=? COLLATE NOCASE", change.table).Scan(&seq); err != nil {
			return err
		}
	}
	// The schema has a short-lived gap between DROP and RENAME. Views and
	// foreign keys elsewhere must continue referring to the original name.
	if _, err := tx.Exec("DROP TABLE " + original); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA legacy_alter_table=ON"); err != nil {
		return err
	}
	if _, err := tx.Exec("ALTER TABLE " + quoted(shadow) + " RENAME TO " + original); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA legacy_alter_table=OFF"); err != nil {
		return err
	}
	if seq.Valid {
		if _, err := tx.Exec("UPDATE sqlite_sequence SET seq=max(seq,?) WHERE name=?", seq.Int64, originalName); err != nil {
			return err
		}
	}
	for _, definition := range objects {
		if _, err := tx.Exec(definition); err != nil {
			return err
		}
	}
	var violation bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check WHERE "table"=? COLLATE NOCASE OR parent=? COLLATE NOCASE)`, change.table, change.table).Scan(&violation); err != nil {
		return err
	}
	if violation {
		return errors.New("foreign key mismatch after column conversion")
	}
	return nil
}

func schemaObjects(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(`SELECT sql FROM sqlite_schema WHERE tbl_name=? AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY rowid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			return nil, err
		}
		if !singleSchemaObject(definition) {
			return nil, errors.New("invalid index or trigger definition")
		}
		objects = append(objects, definition)
	}
	return objects, rows.Err()
}

// SQLite casts malformed or overflowing decimal strings to 0 or a limit.
// Reject those rows instead of changing their meaning in a media task.
func (change historicalTypes) validateValues(tx *sql.Tx, name string) error {
	col := quoted(name)
	if change.to == "TEXT" {
		// Offsets may already contain text ranges or fractional numbers.
		// Refuse a numeric value only if SQLite cannot round-trip its text.
		var invalid bool
		err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM " + quoted(change.table) + " WHERE typeof(" + col + ")='blob' OR (typeof(" + col + ")='real' AND CAST(CAST(" + col + " AS TEXT) AS REAL) IS NOT " + col + "))").Scan(&invalid)
		if err != nil {
			return err
		}
		if invalid {
			return fmt.Errorf("column %s contains a value that cannot be converted to TEXT without loss", name)
		}
		return nil
	}
	rows, err := tx.Query("SELECT typeof(" + col + "), " + col + " FROM " + quoted(change.table) + " WHERE " + col + " IS NOT NULL")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var storage, value string
		if err := rows.Scan(&storage, &value); err != nil {
			return err
		}
		if storage == "integer" {
			continue
		}
		if storage == "text" {
			if _, err := strconv.ParseInt(strings.Trim(value, " \t\r\n\v\f"), 10, 64); err == nil {
				continue
			}
		}
		return fmt.Errorf("column %s contains a value that cannot be converted to %s without loss", name, change.to)
	}
	return rows.Err()
}
