package searchresults

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	_ "modernc.org/sqlite"
)

// Command is a local bridge for processes sharing the existing configuration
// volume. The private row contents travel through stdin/stdout only.
func Command(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	var q struct {
		ID   *int64
		Rows []row
	}
	input := &io.LimitedReader{R: in, N: 64 << 20}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return errInput
	}
	mode := "ro"
	switch op {
	case "list", "get", "grouped":
		if q.Rows != nil || (op == "get") != (q.ID != nil) {
			return errInput
		}
	case "replace", "append":
		if q.Rows == nil || q.ID != nil {
			return errInput
		}
		mode = "rw"
	default:
		return errInput
	}
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dsn := url.URL{Scheme: "file", Path: dbPath}
	params := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)"}}
	if mode == "rw" {
		params.Set("_txlock", "immediate")
	}
	dsn.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	if mode == "ro" {
		rows, err := readRows(ctx, db, q.ID)
		if err != nil {
			return err
		}
		if op == "grouped" {
			return json.NewEncoder(out).Encode(grouped(rows))
		}
		return json.NewEncoder(out).Encode(rows)
	}
	if err := writeRows(ctx, db, q.Rows, op == "replace"); err != nil {
		return err
	}
	_, err = io.WriteString(out, "true\n")
	return err
}
