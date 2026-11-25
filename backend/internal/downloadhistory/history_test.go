package downloadhistory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

func TestClassicHistoryUsesTaskIdentityAndPreservesOldRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO DOWNLOAD_HISTORY(ID,TITLE,ENCLOSURE,DOWNLOADER,DOWNLOAD_ID,DATE,VOTE) VALUES(20,'Film','same','1','a','2000',NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	m := Media{}
	for _, k := range fields {
		m[k] = nil
	}
	str := func(s string) *string { return &s }
	m["TITLE"], m["TMDBID"], m["ENCLOSURE"] = str("Film"), str("77"), str("same")
	write := func(task string) error {
		req, _ := json.Marshal(query{Media: m, Downloader: str("2"), Task: &task, Path: str("/library/Film")})
		return Command(t.Context(), path, "record", bytes.NewReader(req), new(bytes.Buffer))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := write("MixedCase"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := write("another"); err != nil {
		t.Fatal(err)
	}
	read := func(op string, q query) []map[string]any {
		t.Helper()
		req, _ := json.Marshal(q)
		var out bytes.Buffer
		if err := Command(t.Context(), path, op, bytes.NewReader(req), &out); err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		if json.Unmarshal(out.Bytes(), &rows) != nil {
			t.Fatal("invalid reply")
		}
		return rows
	}
	title := read("title", query{Title: str("Film")})
	if len(title) != 3 || title[0]["ID"] != float64(20) || title[0]["VOTE"] != nil {
		t.Fatal(title)
	}
	byID := read("id", query{ID: 20})
	if len(byID) != 1 || byID[0]["DOWNLOAD_ID"] != "a" {
		t.Fatal(byID)
	}
	got := read("task", query{Downloader: str("2"), Task: str("MixedCase")})
	if len(got) != 1 || got[0]["SAVE_PATH"] != "/library/Film" {
		t.Fatal(got)
	}
	if _, err := db.Exec("UPDATE DOWNLOAD_HISTORY SET DATE='2026' WHERE DOWNLOADER='2'"); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]map[string]any{read("list", query{Page: 1, Num: 30}), read("path", query{Path: str("/library/Film")}), read("list", query{Date: str("2025")})} {
		if len(row) != 1 || row[0]["DOWNLOAD_ID"] != "another" {
			t.Fatal(row)
		}
	}
	if len(read("list", query{Page: 2, Num: 1})) != 0 {
		t.Fatal("duplicate title on next page")
	}
}

func TestClassicNoticeKeepsDisplayOrderAndFormatting(t *testing.T) {
	n := Notice{Title: "剧 S02E03", Stars: "评分：8", Source: "自定义订阅", Setting: "高清", Downloader: "家中", User: "乙", Site: "任务名", TaskSite: true, Quality: "4K", Size: "3G", Torrent: "release", Seeders: "6", Promotion: "免费", HR: true, Description: "<b>甲</b>\n乙"}
	title, body := n.Render()
	if title != "剧 S02E03 开始下载" || body != "评分：8\n来自：自定义订阅\n下载设置：高清\n下载器：家中\n用户：乙\n任务：任务名\n质量：4K\n大小：3G\n种子：release\n做种数：6\n促销：免费\nHit&Run：是\n描述：甲\n乙" {
		t.Fatal(title, body)
	}
}
