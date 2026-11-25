package searchresults

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/database"
)

type fixture struct {
	path string
	db   *sql.DB
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return fixture{filepath.Join(dir, "config.yaml"), db}
}
func (f fixture) call(t *testing.T, op string, q any) []byte {
	t.Helper()
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Command(t.Context(), f.path, op, bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func example(title string) row {
	r := row{}
	for _, k := range columns {
		r[k] = nil
	}
	r["TITLE"] = title
	r["TORRENT_NAME"] = title
	r["YEAR"] = "2026"
	r["SIZE"] = int64(4) << 30
	r["RES_TYPE"] = map[string]any{"respix": "2160p", "restype": "WEB-DL", "reseffect": nil}
	r["TYPE"] = "TV"
	r["ES_STRING"] = "S02 E01"
	r["NOTE"] = "中字|fixture|官方"
	r["UPLOAD_VOLUME_FACTOR"] = 1.
	r["DOWNLOAD_VOLUME_FACTOR"] = 0.
	return r
}
func TestSavedResultReplacementAndRollback(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec("INSERT INTO SEARCH_RESULT_INFO(ID,TITLE,SIZE) VALUES (82,'旧',7)"); err != nil {
		t.Fatal(err)
	}
	var rows []row
	if err := json.Unmarshal(f.call(t, "get", row{"id": 82}), &rows); err != nil || len(rows) != 1 || rows[0]["SIZE"] != float64(7) {
		t.Fatal(rows, err)
	}
	a := example("新")
	a["TMDBID"] = json.Number("9007199254740993")
	f.call(t, "replace", row{"rows": []row{a}})
	if string(f.call(t, "get", row{"id": 82})) != "[]\n" {
		t.Fatal("expired ID remained")
	}
	body := f.call(t, "get", row{"id": 83})
	for _, field := range []string{`"SIZE":"4.0G"`, `"TMDBID":"9007199254740993"`, `"DOWNLOAD_VOLUME_FACTOR":0.0`} {
		if !bytes.Contains(body, []byte(field)) {
			t.Fatalf("missing %s", field)
		}
	}
	if _, err := f.db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON SEARCH_RESULT_INFO WHEN NEW.TITLE='bad' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	batch, _ := json.Marshal(row{"rows": []row{a, example("bad")}})
	if Command(t.Context(), f.path, "replace", bytes.NewReader(batch), io.Discard) == nil {
		t.Fatal("partial save accepted")
	}
	if got := f.call(t, "list", row{}); !bytes.Equal(got, body) {
		t.Fatal("prior rows changed")
	}
	for _, q := range []string{`{}`, `{"rows":null}`, `{"rows":[],"unknown":1}`, `{"rows":[{}]}`, `{"rows":[]}{}`} {
		if Command(t.Context(), f.path, "replace", strings.NewReader(q), io.Discard) == nil {
			t.Fatal("accepted", q)
		}
	}
	f.call(t, "replace", row{"rows": []row{}})
	f.call(t, "append", row{"rows": []row{a, a}})
	if got := f.call(t, "get", row{"id": 84}); bytes.Equal(got, []byte("[]\n")) {
		t.Fatal("sequence lost on rollback or clear")
	}
	if got := f.call(t, "get", row{"id": 83}); !bytes.Equal(got, []byte("[]\n")) {
		t.Fatal("stale ID reused")
	}
}

func TestConcurrentResultWritersAndReaders(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := example(fmt.Sprint(i))
			b, _ := json.Marshal(row{"rows": []row{r, r, r}})
			if err := Command(t.Context(), f.path, "replace", bytes.NewReader(b), io.Discard); err != nil {
				t.Error(err)
				return
			}
			var out bytes.Buffer
			err := Command(t.Context(), f.path, "list", strings.NewReader("{}"), &out)
			var rows []row
			if err != nil || json.Unmarshal(out.Bytes(), &rows) != nil || len(rows) != 3 || rows[0]["TITLE"] != rows[2]["TITLE"] {
				t.Errorf("partial batch: %v %v", rows, err)
			}
		}(i)
	}
	wg.Wait()
	var high int
	if err := f.db.QueryRow("SELECT last_id FROM GO_SEARCH_RESULT_SEQUENCE").Scan(&high); err != nil || high != 24 {
		t.Fatal(high, err)
	}
}

func TestGroupedResultOrderingAndFilters(t *testing.T) {
	f := newFixture(t)
	z, a := example("Z"), example("A")
	pack := example("Z")
	pack["ES_STRING"] = "S02"
	paid := example("Z")
	paid["DOWNLOAD_VOLUME_FACTOR"] = 0.04
	paid["OTHERINFO"] = "Team"
	paid["SITE"] = "second"
	a["DOWNLOAD_VOLUME_FACTOR"] = nil
	f.call(t, "replace", row{"rows": []row{z, a, paid, pack, z}})
	if _, err := f.db.Exec("INSERT INTO SEARCH_RESULT_INFO(RES_TYPE,TITLE) VALUES ('broken','ignored')"); err != nil {
		t.Fatal(err)
	}
	raw := f.call(t, "grouped", row{})
	input := string(raw)
	if strings.Index(input, `"Z (2026)":`) > strings.Index(input, `"A (2026)":`) || strings.Contains(input, "ignored") {
		t.Fatal("order or corrupt row")
	}
	var page struct {
		Total  int
		Result map[string]struct {
			Filter  facets
			Seasons [][]json.RawMessage `json:"torrent_dict"`
		}
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	zview := page.Result["Z (2026)"]
	if page.Total != 6 || len(zview.Seasons) != 2 || string(zview.Seasons[0][0]) != `"S02"` || strings.Join(zview.Filter.Teams, "/") != "Team/未知" || len(zview.Filter.Free) != 2 || zview.Filter.Free[0].Name != "免费" || zview.Filter.Free[1].Name != "未知" {
		t.Fatal(zview, page.Total)
	}
	if got := page.Result["A (2026)"].Filter.Free[0].Value; got != "1.0 None" {
		t.Fatal(got)
	}
	var groups map[string]struct {
		Total    int                `json:"group_total"`
		Variants map[string]variant `json:"group_torrents"`
	}
	if err := json.Unmarshal(zview.Seasons[1][1], &groups); err != nil {
		t.Fatal(err)
	}
	g := groups["2160p_webdl"]
	v := g.Variants["2160p_webdl___40g_none"]
	if g.Total != 3 || len(v.Torrents) != 2 || v.Torrents[0]["id"] != float64(1) || v.Torrents[1]["id"] != float64(5) {
		t.Fatal(g)
	}
}

func TestExistingAutoincrementAfterClear(t *testing.T) {
	f := newFixture(t)
	var schema string
	if err := f.db.QueryRow("SELECT sql FROM sqlite_schema WHERE name='SEARCH_RESULT_INFO'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	schema = strings.Replace(schema, `"ID" INTEGER NOT NULL`, `"ID" INTEGER PRIMARY KEY AUTOINCREMENT`, 1)
	schema = strings.Replace(schema, ",\n\tPRIMARY KEY (\"ID\")", "", 1)
	if _, err := f.db.Exec("DROP TABLE SEARCH_RESULT_INFO;" + schema + `; INSERT INTO SEARCH_RESULT_INFO (ID,TITLE) VALUES (310,'old'); DELETE FROM SEARCH_RESULT_INFO`); err != nil {
		t.Fatal(err)
	}
	f.call(t, "replace", row{"rows": []row{example("new")}})
	if got := string(f.call(t, "get", row{"id": 311})); !strings.Contains(got, `"TITLE":"new"`) {
		t.Fatal(got)
	}
}
