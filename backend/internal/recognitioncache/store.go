package recognitioncache

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const expiryField = "cache_expire_timestamp"
const lifetime = int64(7 * 24 * time.Hour / time.Second)
const negativeLifetime = int64(12 * time.Hour / time.Second)

var ErrChanged = errors.New("recognition cache entry changed")
var ErrRequest = errors.New("invalid recognition cache request")

type Entry map[string]any
type Request struct {
	Key, Title, Search, ID string
	Page, Size             int
	Entries                map[string]Entry
	Expected               *ExpectedEntry
}

type ExpectedEntry struct {
	ID, Title string
}
type ListItem struct {
	Key   string `json:"key"`
	Entry Entry  `json:"entry"`
}
type Page struct {
	Total int        `json:"total"`
	Items []ListItem `json:"items"`
}

func knownType(s string) bool {
	return s == "电影" || s == "电视剧" || s == "动漫" || s == "未知"
}
func (e Entry) ID() string { return e.Field("id") }

func (e Entry) Field(name string) string {
	switch v := e[name].(type) {
	case string:
		return v
	case json.Number, int, int64, float64:
		return fmt.Sprint(v)
	default:
		return ""
	}
}
func validKey(s string) bool { return s != "" && len(s) <= 64<<10 && !strings.ContainsRune(s, 0) }
func encode(e Entry) (string, error) {
	if id, err := strconv.ParseInt(e.ID(), 10, 64); err != nil || id < 0 {
		return "", ErrRequest
	}
	size := 0
	for k, v := range e {
		if !validKey(k) {
			return "", ErrRequest
		}
		size += len(k)
		switch value := v.(type) {
		case string:
			size += len(value)
		case nil, bool, int, int64, json.Number, float64:
		default:
			return "", ErrRequest
		}
		if size > 256<<10 {
			return "", ErrRequest
		}
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 256<<10 {
		return "", ErrRequest
	}
	return string(raw), nil
}
func decode(raw string) (Entry, error) {
	var e Entry
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&e)
	return e, err
}

func importLegacy(ctx context.Context, tx *sql.Tx, path string, now int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > legacyLimit {
		return errLegacy
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > legacyLimit {
		return errLegacy
	}
	data, err := io.ReadAll(io.LimitReader(f, legacyLimit+1))
	if err != nil {
		return err
	}
	d, err := readLegacy(data)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO GO_RECOGNITION_CACHE(CACHE_KEY,MEDIA_ID,DOCUMENT,EXPIRES) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	total := 0
	for _, k := range d.keys {
		record, ok := d.values[k].(*legacyDict)
		if !ok || !validKey(k) {
			return errLegacy
		}
		e := Entry(record.values)
		exp := now + lifetime
		if v := e[expiryField]; v != nil {
			exp, err = strconv.ParseInt(e.Field(expiryField), 10, 64)
			if err != nil {
				return errLegacy
			}
		}
		raw, err := encode(e)
		if err != nil {
			return errLegacy
		}
		total += len(k) + len(raw)
		if total > legacyLimit*2 {
			return errLegacy
		}
		if e.ID() == "0" {
			continue
		} // Old process-local negative lookups were never saved.
		if _, err = stmt.ExecContext(ctx, k, e.ID(), raw, exp); err != nil {
			return err
		}
	}
	return nil
}

func open(ctx context.Context, path string, clear bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_txlock=immediate&_pragma=busy_timeout(30000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = initialize(ctx, db, path, clear); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type cacheReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func initialized(ctx context.Context, r cacheReader) (bool, error) {
	var ok bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_RECOGNITION_CACHE')`).Scan(&ok)
	return ok, err
}
func initialize(ctx context.Context, db *sql.DB, path string, clear bool) error {
	if ok, err := initialized(ctx, db); err != nil || ok {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if ok, err := initialized(ctx, tx); err != nil || ok {
		return err
	}
	for _, query := range []string{
		`CREATE TABLE GO_RECOGNITION_CACHE(CACHE_KEY TEXT PRIMARY KEY,MEDIA_ID TEXT NOT NULL,DOCUMENT TEXT NOT NULL,EXPIRES INTEGER NOT NULL)`,
		`CREATE INDEX GO_RECOGNITION_CACHE_ID ON GO_RECOGNITION_CACHE(MEDIA_ID,EXPIRES)`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	if !clear {
		if err := importLegacy(ctx, tx, filepath.Join(filepath.Dir(path), "tmdb.dat"), time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func readEntry(ctx context.Context, tx cacheReader, key string) (Entry, int64, error) {
	var raw string
	var exp int64
	err := tx.QueryRowContext(ctx, "SELECT DOCUMENT,EXPIRES FROM GO_RECOGNITION_CACHE WHERE CACHE_KEY=?", key).Scan(&raw, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	e, err := decode(raw)
	if e != nil {
		e[expiryField] = exp
	}
	return e, exp, err
}
func apply(ctx context.Context, tx *sql.Tx, op string, q Request, expire bool, now int64) (any, error) {
	if op == "get" || op == "rename" || op == "delete" {
		if !validKey(q.Key) {
			return nil, ErrRequest
		}
		e, exp, err := readEntry(ctx, tx, q.Key)
		if err != nil {
			return nil, err
		}
		if q.Expected != nil && (e == nil || e.ID() != q.Expected.ID || e.Field("title") != q.Expected.Title) {
			return nil, ErrChanged
		}
		if e == nil {
			return nil, nil
		}
		switch op {
		case "get":
			if exp <= now && (expire || e.ID() == "0") {
				_, err = tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE CACHE_KEY=?", q.Key)
				return nil, err
			}
			if e.ID() != "0" && exp != now+lifetime {
				exp = now + lifetime
				_, err = tx.ExecContext(ctx, "UPDATE GO_RECOGNITION_CACHE SET EXPIRES=? WHERE CACHE_KEY=?", exp, q.Key)
				e[expiryField] = exp
			}
		case "delete":
			_, err = tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE CACHE_KEY=?", q.Key)
		case "rename":
			if strings.TrimSpace(q.Title) == "" || len(q.Title) > 64<<10 {
				return nil, ErrRequest
			}
			e["title"] = q.Title
			e[expiryField] = now + lifetime
			var raw string
			raw, err = encode(e)
			if err == nil {
				_, err = tx.ExecContext(ctx, "UPDATE GO_RECOGNITION_CACHE SET DOCUMENT=?,EXPIRES=? WHERE CACHE_KEY=?", raw, now+lifetime, q.Key)
			}
		}
		return e, err
	}
	switch op {
	case "put":
		keys := make([]string, 0, len(q.Entries))
		for k := range q.Entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if _, err := tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE MEDIA_ID='0' AND EXPIRES<=?", now); err != nil {
			return nil, err
		}
		for _, k := range keys {
			e := q.Entries[k]
			if !validKey(k) || e == nil {
				return nil, ErrRequest
			}
			if expire {
				if _, err := tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE CACHE_KEY=? AND EXPIRES<=?", k, now); err != nil {
					return nil, err
				}
			}
			exp := now + lifetime
			if e.ID() == "0" {
				exp = now + negativeLifetime
			}
			e[expiryField] = exp
			raw, err := encode(e)
			if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO GO_RECOGNITION_CACHE(CACHE_KEY,MEDIA_ID,DOCUMENT,EXPIRES) VALUES(?,?,?,?) ON CONFLICT(CACHE_KEY) DO NOTHING`, k, e.ID(), raw, exp); err != nil {
				return nil, err
			}
		}
	case "clear":
		_, err := tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE")
		return nil, err
	case "delete-id", "clear-unknown":
		id := q.ID
		if op == "clear-unknown" {
			id = "0"
		}
		if id == "" {
			return nil, ErrRequest
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE MEDIA_ID=?", id)
		return nil, err
	case "list":
		if q.Page < 1 || q.Size < 1 || q.Size > 10000 {
			return nil, ErrRequest
		}
		if expire {
			if _, err := tx.ExecContext(ctx, "DELETE FROM GO_RECOGNITION_CACHE WHERE EXPIRES<=?", now); err != nil {
				return nil, err
			}
		}
		out := Page{Items: []ListItem{}}
		rows, err := tx.QueryContext(ctx, `SELECT CACHE_KEY,DOCUMENT FROM GO_RECOGNITION_CACHE WHERE MEDIA_ID!='0' ORDER BY rowid`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		find := strings.ToLower(q.Search)
		for rows.Next() {
			var k, raw string
			if err := rows.Scan(&k, &raw); err != nil {
				return nil, err
			}
			if strings.Contains(strings.ToLower(k), find) {
				out.Total++
				if (out.Total-1)/q.Size == q.Page-1 {
					e, err := decode(raw)
					if err != nil {
						return nil, err
					}
					out.Items = append(out.Items, ListItem{k, e})
				}
			}
		}
		return out, rows.Err()
	default:
		return nil, ErrRequest
	}
	return nil, nil
}

func Run(ctx context.Context, path, op string, q Request, expire bool) (any, error) {
	db, err := open(ctx, path, op == "clear")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := apply(ctx, tx, op, q, expire, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// Lookup also serves read-only recognition previews. A miss on a fresh
// installation leaves the database untouched. A legacy file is imported once.
func Lookup(ctx context.Context, path, key string, expire bool) (Entry, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(30000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ok, err := initialized(ctx, db)
	if err != nil {
		return nil, err
	}
	if !ok {
		if _, err = os.Stat(filepath.Join(filepath.Dir(path), "tmdb.dat")); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		result, err := Run(ctx, path, "get", Request{Key: key}, expire)
		e, _ := result.(Entry)
		return e, err
	}
	e, exp, err := readEntry(ctx, db, key)
	if err != nil || e == nil {
		return nil, err
	}
	if exp <= time.Now().Unix() && (expire || e.ID() == "0") {
		return nil, nil
	}
	return e, nil
}

func ReadRequest(in io.Reader) (Request, error) {
	var q Request
	data, err := io.ReadAll(io.LimitReader(in, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return q, ErrRequest
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF {
		return q, ErrRequest
	}
	return q, nil
}
