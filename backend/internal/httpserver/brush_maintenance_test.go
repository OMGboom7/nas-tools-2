package httpserver

import (
	"database/sql"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"path/filepath"
	"testing"
	"time"
)

func TestBrushMaintenanceRuleOrderUnitsAndPhases(t *testing.T) {
	now := time.Date(2026, 11, 27, 13, 55, 0, 0, time.FixedZone("UTC+8", 8*3600))
	limits, e := brushRemovalLimits(`{'time':'gt#2','ratio':'gt#0.5','uploadsize':'gt#1','dltime':'gt#1','avg_upspeed':'lt#2','iatime':'gt#3'}`)
	if e != nil {
		t.Fatal(e)
	}
	decide := func(o brushObservation) string {
		return brushMaintenanceDecision(o, limits, "", false, false, now).Reason
	}
	o := brushObservation{Completed: true, SeedSeconds: 7201, Ratio: .51, Uploaded: (1 << 30) + 1, AgeSeconds: 3601, AverageUpload: 2047, InactiveSeconds: 10801}
	if decide(o) != "做种时间" {
		t.Fatal(o)
	}
	o.SeedSeconds--
	if decide(o) != "分享率" {
		t.Fatal(o)
	}
	o.Ratio = .5
	if decide(o) != "上传量" {
		t.Fatal(o)
	}
	o.Uploaded--
	if decide(o) != "平均上传速度" {
		t.Fatal(o)
	}
	o.AverageUpload++
	if decide(o) != "未活动时间" {
		t.Fatal(o)
	}
	o.InactiveSeconds--
	if decide(o) != "" {
		t.Fatal(o)
	}
	o.Completed = false
	o.Size = o.Uploaded*2 - 1
	if decide(o) != "分享率" {
		t.Fatal(o)
	}
	o.Size++
	if decide(o) != "下载耗时" {
		t.Fatal(o)
	}
	o.AgeSeconds--
	o.AverageUpload = 0
	if decide(o) != "" {
		t.Fatal(o)
	}
	for _, end := range []string{"20261127_1400", "2026-11-27 14:00:00"} {
		if d := brushMaintenanceDecision(o, nil, end, true, true, now); d.Action != "remove" || d.Reason != "限免结束" {
			t.Fatal(d)
		}
		if d := brushMaintenanceDecision(o, nil, end, false, true, now); d.Action != "throttle" {
			t.Fatal(d)
		}
		if d := brushMaintenanceDecision(o, nil, end, true, true, now.Add(-time.Second)); d.Action != "keep" {
			t.Fatal(d)
		}
	}
	o.Size = 0
	if d := brushMaintenanceDecision(o, nil, "20261127_1400", true, true, now); d.Action != "keep" {
		t.Fatal(d)
	}
	o.Completed = true
	if d := brushMaintenanceDecision(o, nil, "20261127_1400", true, true, now); d.Action != "keep" {
		t.Fatal(d)
	}
	for _, bad := range []string{"lt#1", "gt#-1", "gt#NaN", "gt#Inf", "gt#0#1"} {
		if _, e := brushRemovalLimits(`{"time":"` + bad + `"}`); e == nil {
			t.Fatal(bad)
		}
	}
}

func TestBrushMaintenanceRejectsStaleAndAmbiguousHistory(t *testing.T) {
	dir := t.TempDir()
	if e := database.PrepareApplicationDatabases(dir); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	exec := func(s string) {
		t.Helper()
		if _, e := db.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO SITE_BRUSH_TASK(ID,DOWNLOADER,STATE,REMOVE_RULE) VALUES(4,'8','S','{"ratio":"gt#1"}');
 INSERT INTO SITE_BRUSH_TORRENTS(TASK_ID,DOWNLOADER,DOWNLOAD_ID) VALUES('4','8','fixture');`)
	q := brushMaintenanceRequest{TaskID: 4, Downloader: "8", Items: []brushObservation{{ID: "fixture", Completed: true, Ratio: 1.1}}}
	plan := func() (map[string]brushDecision, error) { return planBrushMaintenance(t.Context(), db, q, time.Now()) }
	if m, e := plan(); e != nil || m["fixture"].Action != "remove" {
		t.Fatal(m, e)
	}
	exec("UPDATE SITE_BRUSH_TASK SET REMOVE_RULE='{}'")
	if m, e := plan(); e != nil || m["fixture"].Action != "keep" {
		t.Fatal(m, e)
	}
	exec("INSERT INTO SITE_BRUSH_TORRENTS(TASK_ID,DOWNLOADER,DOWNLOAD_ID) VALUES('5','8','fixture')")
	if _, e := plan(); e == nil {
		t.Fatal("ambiguous task")
	}
	exec("DELETE FROM SITE_BRUSH_TORRENTS WHERE TASK_ID='5'")
	exec("UPDATE SITE_BRUSH_TASK SET STATE='N'")
	if _, e := plan(); e == nil {
		t.Fatal("disabled task")
	}
	exec("UPDATE SITE_BRUSH_TASK SET STATE='Y', DOWNLOADER='9'")
	if _, e := plan(); e == nil {
		t.Fatal("changed downloader")
	}
}
