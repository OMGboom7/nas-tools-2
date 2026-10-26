//go:build linux || darwin

package organization

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func mediaRefreshOrgControl(t *testing.T, s *Store) mediaserver.RefreshControl {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); err != nil {
		t.Fatal(err)
	}
	v := map[string]any{"enable": true, "delay": "0"}
	c := mediaserver.RefreshControl{Epoch: strings.Repeat("a", 32), Actor: "admin", Server: "emby", ServerID: "server-1", ServerDigest: strings.Repeat("b", 64), AuthorityDigest: strings.Repeat("c", 64), PluginDigest: mediaserver.RefreshDigest(v), Created: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), Authorized: true}
	for key, value := range map[string]any{"UserInstalledPlugins": []string{"LibraryRefresh"}, mediaserver.RefreshPluginKey: v, mediaserver.RefreshControlKey: c} {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}
func TestMediaRefreshFrozenPayloadKeepsAllLocalRemoteModesTypesAndWholeDiscBoundaries(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move", "rclonecopy", "rclone", "miniocopy", "minio"} {
		for _, tc := range []struct{ stored, want string }{{"电影", "MOV"}, {"电视剧", "TV"}, {"动漫", "ANIME"}} {
			t.Run(mode+"-"+tc.want, func(t *testing.T) {
				d, e := copyFixture(t)
				d.Mode = mode
				e.MediaType = tc.stored
				e.Category = "Category"
				d.Entries = []Entry{e, {Kind: "subtitle", Target: "ignored.srt"}}
				items := mediaRefreshItems(d)
				if len(items) != 1 || items[0].Type != tc.want || items[0].TargetPath != filepath.ToSlash(filepath.Join(d.TargetRoot, e.Target)) || items[0].Directory || !mediaserver.ValidRefreshItems(items) {
					t.Fatal(items)
				}
				e.Disc = &DiscInfo{}
				d.Entries = []Entry{e}
				items = mediaRefreshItems(d)
				if !items[0].Directory {
					t.Fatal("whole disc became an internal clip", items)
				}
			})
		}
	}
}
func TestMediaRefreshOutboxAndOriginalSuccessReceiptShareOneAtomicTransaction(t *testing.T) {
	d, e := copyFixture(t)
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := mediaRefreshOrgControl(t, s)
	plan := mediaserver.RefreshPlan{Server: "emby", ServerID: "server-1", Targets: []mediaserver.RefreshTarget{{Kind: "root", ID: "root"}}}
	_, err = s.MediaRefresh().Create(t.Context(), mediaserver.RefreshDefinition{Actor: c.Actor, RequestID: strings.Repeat("d", 32), Source: "refresh_media_server", Whole: true, Items: []mediaserver.RefreshItem{}, Control: c, Reviewed: &plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_auto_refresh BEFORE INSERT ON GO_MEDIA_REFRESH_JOBS WHEN json_extract(NEW.DOCUMENT,'$.Source')='transfer_finished' BEGIN SELECT RAISE(ABORT,'fixture');END`); err != nil {
		t.Fatal(err)
	}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MediaRefreshSource(t.Context(), id); err == nil {
		t.Fatal("unsuccessful intake adopted")
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	p, err := Copy(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(t.Context(), job, job.Items[0]); err == nil {
		t.Fatal("outbox failure reported successful intake")
	}
	old, err := s.Get(t.Context(), id)
	if err != nil || old.Items[0].State != "prepared" {
		t.Fatal(old, err)
	}
	if err := VerifyPublished(t.Context(), d, e, p); err != nil {
		t.Fatal("target changed on database failure", err)
	}
	for _, table := range []string{"TRANSFER_HISTORY", "GO_BUSINESS_EVENTS"} {
		yes, err := recoveryTable(t.Context(), s.db, table)
		if err != nil {
			t.Fatal(err)
		}
		if yes {
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial original success", table, count, err)
			}
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_auto_refresh`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Complete(t.Context(), job, job.Items[0]); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.MediaRefresh().List(t.Context(), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	automatic := 0
	for _, r := range rows {
		if r.Source == "transfer_finished" {
			automatic++
			if r.Reference != id || r.State != "pending" || len(r.Definition.Items) != 1 {
				t.Fatal(r)
			}
		}
	}
	if automatic != 1 {
		t.Fatal("automatic event duplicated", automatic)
	}
	items, err := s.MediaRefreshSource(t.Context(), id)
	if err != nil || len(items) != 1 || items[0].TargetPath != filepath.ToSlash(filepath.Join(d.TargetRoot, e.Target)) {
		t.Fatal(items, err)
	}
}
