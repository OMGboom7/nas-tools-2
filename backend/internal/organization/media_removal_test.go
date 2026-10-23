//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func mediaRemovalHistoryFixture(t *testing.T, x *recoveryUnit, id int64) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT)`,
		`CREATE TABLE IF NOT EXISTS TRANSFER_BLACKLIST(ID INTEGER PRIMARY KEY,PATH TEXT)`,
		`CREATE TABLE IF NOT EXISTS SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT)`,
	} {
		if _, err := x.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	filename := "original-" + strconv.FormatInt(id, 10) + ".mkv"
	if _, err := x.s.db.Exec(`INSERT INTO TRANSFER_HISTORY(ID,MODE,TYPE,TMDBID,TITLE,YEAR,SOURCE_PATH,SOURCE_FILENAME,DEST,DEST_PATH,DEST_FILENAME,DATE)VALUES(?,'复制','电影',100,'Movie','2026','/original',?,?,?,'movie.mkv','2026-10-23')`, id, filename, x.scope.Root.Path, path.Join(x.scope.Root.Path, "Movie")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO TRANSFER_BLACKLIST(ID,PATH)VALUES(?,?)`, `INSERT INTO SYNC_HISTORY(ID,PATH)VALUES(?,?)`} {
		if _, err := x.s.db.Exec(q, id, "/original/"+filename); err != nil {
			t.Fatal(err)
		}
	}
}
func mediaRemovalTestUnit(t *testing.T, x *recoveryUnit, selected, role string, history int64) MediaRemovalUnit {
	t.Helper()
	present, err := MediaRemovalPresence(t.Context(), x.scope, selected, x.c)
	if err != nil {
		t.Fatal(err)
	}
	u := MediaRemovalUnit{Definition: RecoveryDefinition{Scope: x.scope, Path: selected, Removal: &MediaRemovalOrigin{Role: role}}, Absent: !present, Events: []MediaRemovalEventRef{}}
	if present {
		u.Definition.Nodes, err = MediaRemovalSnapshot(t.Context(), x.scope, selected, x.c)
		if err != nil {
			t.Fatal(err)
		}
		if history > 0 {
			u.Events = append(u.Events, MediaRemovalEventRef{HistoryID: strconv.FormatInt(history, 10), Role: role, Path: selected})
		}
	}
	return u
}
func mediaRemovalTestPlan(t *testing.T, x *recoveryUnit, units []MediaRemovalUnit, ids []int64) MediaRemovalReview {
	t.Helper()
	d := MediaRemovalDefinition{Actor: "admin", RequestID: Digest([]any{ids, units})[:32], Decision: "complete independently reviewed historical media removal", InputDigest: Digest([]any{ids, units}), Units: units}
	v, err := x.s.PrepareMediaRemoval(t.Context(), d, ids)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mediaRemovalTestSave(t *testing.T, x *recoveryUnit, v MediaRemovalReview) MediaRemovalReview {
	t.Helper()
	id, err := x.s.CreateMediaRemoval(t.Context(), v.Definition, v.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	v, err = x.s.MediaRemoval(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mediaRemovalTestReview(t *testing.T, x *recoveryUnit, id string) MediaRemovalReview {
	t.Helper()
	v, err := x.s.MediaRemoval(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	clients := make([]*remotestore.Client, len(v.Units))
	for i := range clients {
		clients[i] = x.c
	}
	v, err = x.s.ReviewMediaRemoval(t.Context(), v, clients)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mediaRemovalTestExecute(t *testing.T, x *recoveryUnit, v MediaRemovalReview) error {
	t.Helper()
	clients := make([]*remotestore.Client, len(v.Units))
	for i := range clients {
		clients[i] = x.c
	}
	return x.s.ExecuteMediaRemoval(t.Context(), v, clients, v.Fingerprint, func(RecoveryScope) error { return nil })
}
func TestMediaRemovalAllProvidersCompleteMediaAndExactHistoryDedupeTransaction(t *testing.T) {
	for _, driver := range []string{"", "rclone", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryFixture(t, driver)
			mediaRemovalHistoryFixture(t, x, 1)
			mediaRemovalHistoryFixture(t, x, 2)
			x.put(t, "Movie/movie.mkv", "movie")
			x.put(t, "Movie/movie.nfo", "metadata")
			x.put(t, "keep/other.mkv", "unchanged")
			units := []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1), mediaRemovalTestUnit(t, x, "Movie/movie.nfo", "cleanup", 0)}
			preview := mediaRemovalTestPlan(t, x, units, []int64{1})
			if preview.DedupeRows != 2 || preview.Records[0].ID != "1" {
				t.Fatal(preview)
			}
			v := mediaRemovalTestSave(t, x, preview)
			if v.State != "ready" || !x.exists(t, "Movie/movie.mkv") {
				t.Fatal("save executed deletion")
			}
			v = mediaRemovalTestReview(t, x, v.ID)
			if !v.CanContinue {
				t.Fatal(v.Reason)
			}
			if err := mediaRemovalTestExecute(t, x, v); err != nil {
				t.Fatal(err)
			}
			v = mediaRemovalTestReview(t, x, v.ID)
			if v.State != "completed" || x.exists(t, "Movie/movie.mkv") || x.exists(t, "Movie/movie.nfo") || x.bytes(t, "keep/other.mkv") != "unchanged" {
				t.Fatal(v)
			}
			if _, err := x.s.GetHistory(t.Context(), 1); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			if _, err := x.s.GetHistory(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"TRANSFER_BLACKLIST", "SYNC_HISTORY"} {
				var n int
				if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 1 {
					t.Fatal(table, n, err)
				}
			}
			events, err := x.s.BusinessEvents().List(t.Context())
			if err != nil || len(events) != 1 || events[0].Kind != "library_file_deleted" {
				t.Fatal(events, err)
			}
			var data map[string]any
			if json.Unmarshal(events[0].Data, &data) != nil || data["filename"] != "movie.mkv" || data["history_id"] != "1" {
				t.Fatal(string(events[0].Data))
			}
			// Idempotent save reads the old receipt, even after the historical row
			// is gone, and cannot delete a new row reusing the same old integer ID.
			mediaRemovalHistoryFixture(t, x, 1)
			id, err := x.s.CreateMediaRemoval(t.Context(), preview.Definition, preview.Fingerprint)
			if err != nil || id != v.ID {
				t.Fatal(id, err)
			}
			if _, err := x.s.GetHistory(t.Context(), 1); err != nil {
				t.Fatal("receipt-only retry removed replacement", err)
			}
		})
	}
}
func TestMediaRemovalRawNullIntegerBlobExtraColumnsAndLateDedupeEdits(t *testing.T) {
	x := recoveryFixture(t, "")
	large := int64(9007199254740993)
	mediaRemovalHistoryFixture(t, x, large)
	if _, err := x.s.db.Exec(`ALTER TABLE TRANSFER_HISTORY ADD COLUMN EXTRA_BLOB BLOB`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`UPDATE TRANSFER_HISTORY SET CATEGORY=NULL,TITLE='',EXTRA_BLOB=? WHERE ID=?`, []byte{0, 255, 1}, large); err != nil {
		t.Fatal(err)
	}
	p := mediaRemovalTestPlan(t, x, nil, []int64{large})
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"id":"9007199254740993"`) {
		t.Fatal(string(b))
	}
	rows := p.Definition.Histories[0].Rows
	found := map[string]MediaRemovalValue{}
	for i, col := range rows[0].Columns {
		found[col] = rows[0].Values[i]
	}
	if found["ID"].Int != large || found["CATEGORY"].Kind != "null" || found["TITLE"].Kind != "text" || found["EXTRA_BLOB"].Kind != "blob" || len(found["EXTRA_BLOB"].Blob) != 3 {
		t.Fatal(found)
	}
	if _, err := x.s.db.Exec(`UPDATE SYNC_HISTORY SET SRC='newer edit' WHERE ID=?`, large); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.CreateMediaRemoval(t.Context(), p.Definition, p.Fingerprint); !errors.Is(err, ErrState) {
		t.Fatal("stale dedupe edit accepted", err)
	}
	p = mediaRemovalTestPlan(t, x, nil, []int64{large})
	v := mediaRemovalTestSave(t, x, p)
	v = mediaRemovalTestReview(t, x, v.ID)
	if err := mediaRemovalTestExecute(t, x, v); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := x.s.db.QueryRow(`SELECT DEFINITION FROM GO_MEDIA_REMOVALS WHERE ID=?`, v.ID).Scan(&raw); err != nil || !strings.Contains(raw, `"kind":"blob"`) {
		t.Fatal(raw, err)
	}
}
func TestMediaRemovalHistoryAndDedupeFailureRollsBackAndNeverReplaysCompletedFiles(t *testing.T) {
	x := recoveryFixture(t, "")
	mediaRemovalHistoryFixture(t, x, 1)
	x.put(t, "Movie/movie.mkv", "movie")
	p := mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1})
	v := mediaRemovalTestSave(t, x, p)
	if _, err := x.s.db.Exec(`CREATE TRIGGER refuse_sync_removal BEFORE DELETE ON SYNC_HISTORY BEGIN SELECT RAISE(ABORT,'fixture rollback');END`); err != nil {
		t.Fatal(err)
	}
	if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err == nil {
		t.Fatal("expected final DB refusal")
	}
	if x.exists(t, "Movie/movie.mkv") {
		t.Fatal("file not deleted")
	}
	if _, err := x.s.GetHistory(t.Context(), 1); err != nil {
		t.Fatal("history transaction did not roll back", err)
	}
	for _, table := range []string{"TRANSFER_BLACKLIST", "SYNC_HISTORY"} {
		var n int
		_ = x.s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		if n != 1 {
			t.Fatal(table, n)
		}
	}
	if _, err := x.s.db.Exec(`DROP TRIGGER refuse_sync_removal`); err != nil {
		t.Fatal(err)
	}
	x.s.Close()
	var err error
	x.s, err = OpenStore(x.database)
	if err != nil {
		t.Fatal(err)
	}
	v = mediaRemovalTestReview(t, x, v.ID)
	if !v.CanContinue {
		t.Fatal(v.Reason)
	}
	if err := mediaRemovalTestExecute(t, x, v); err != nil {
		t.Fatal(err)
	}
	events, err := x.s.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
}
func TestMediaRemovalLostDeleteReceiptMinioUsesOneWireDeleteAndRestartRequiresFreshReview(t *testing.T) {
	x := recoveryFixture(t, "minio")
	mediaRemovalHistoryFixture(t, x, 1)
	x.put(t, "Movie/movie.mkv", "movie")
	v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1}))
	key := "bucket/library/Movie/movie.mkv"
	x.cloud.FailNextDelete(key, 500)
	if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err == nil {
		t.Fatal("lost wire receipt not surfaced")
	}
	if _, err := x.s.GetHistory(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if events, err := x.s.BusinessEvents().List(t.Context()); err != nil || len(events) != 0 {
		t.Fatal("unproven deletion emitted event", events, err)
	}
	x.s.Close()
	var err error
	x.s, err = OpenStore(x.database)
	if err != nil {
		t.Fatal(err)
	}
	v = mediaRemovalTestReview(t, x, v.ID)
	if !v.CanContinue {
		t.Fatal(v.Reason)
	}
	if err := mediaRemovalTestExecute(t, x, v); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range x.cloud.Requests() {
		if r.Method == "DELETE" && r.Key == key {
			count++
			if r.IfMatch == "" {
				t.Fatal("unbound delete")
			}
		}
	}
	if count != 1 {
		t.Fatal("lost delete replayed", count)
	}
}
func TestMediaRemovalNewObjectsMissingSelectionsAndNoFakeDeleteEvents(t *testing.T) {
	for _, driver := range []string{"", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryFixture(t, driver)
			mediaRemovalHistoryFixture(t, x, 1)
			x.put(t, "Movie/existing.mkv", "existing")
			missing := mediaRemovalTestUnit(t, x, "Movie/missing.mkv", "library", 1)
			v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{missing}, []int64{1}))
			x.put(t, "Movie/missing.mkv", "new arrival")
			review := mediaRemovalTestReview(t, x, v.ID)
			if review.CanContinue {
				t.Fatal("adopted late arrival")
			}
			if err := mediaRemovalTestExecute(t, x, review); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			if x.bytes(t, "Movie/missing.mkv") != "new arrival" {
				t.Fatal("late arrival changed")
			}
			if _, err := x.s.GetHistory(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			if err := x.s.CloseMediaRemoval(t.Context(), v.ID); err != nil {
				t.Fatal(err)
			}
			closed, err := x.s.MediaRemoval(t.Context(), v.ID)
			if err != nil {
				t.Fatal(err)
			}
			closed, err = x.s.ReviewMediaRemoval(t.Context(), closed, nil)
			if err != nil || closed.State != "closed" {
				t.Fatal(closed, err)
			}
		})
	}
}
func TestMediaRemovalLocalCaptureReturnNeverOverwritesAndClosesOldWriter(t *testing.T) {
	x := recoveryFixture(t, "")
	x.put(t, "Movie/movie.mkv", "original")
	info, _ := os.Stat(filepath.Join(x.root, "Movie/movie.mkv"))
	d := Definition{SourceRoot: x.root, TargetRoot: x.root, SourceIdentity: x.scope.Root.Identity, TargetIdentity: x.scope.Root.Identity, SourceID: "root", TargetID: "root", Mode: "copy", ConfigDigest: Digest("fixture"), Entries: []Entry{{Source: "Movie/movie.mkv", Target: "published/other.mkv", Kind: "media", Size: info.Size(), Identity: Identity(info)}}}
	job, err := x.s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "source", 0)}, nil))
	review := mediaRemovalTestReview(t, x, v.ID)
	if !review.CanContinue {
		t.Fatal(review.Reason)
	}
	// Stop immediately after capture, before the first deletion intent.
	err = x.s.ExecuteMediaRemoval(t.Context(), review, []*remotestore.Client{nil}, review.Fingerprint, func(s RecoveryScope) error {
		child, e := x.s.RecoveryDisposal(t.Context(), mediaRemovalChildID(v.Definition, 0))
		if e == nil {
			if n, _ := recoveryLocalPresence(s, child.Capture); n != nil {
				return context.Canceled
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected interrupted capture")
	}
	review = mediaRemovalTestReview(t, x, v.ID)
	if review.Units[0].Review == nil || !review.Units[0].Review.CanReturn {
		t.Fatal(review)
	}
	x.put(t, "Movie/movie.mkv", "late original position")
	blocked := mediaRemovalTestReview(t, x, v.ID)
	if blocked.Units[0].Review.CanReturn {
		t.Fatal("would overwrite late object")
	}
	if err := os.Remove(filepath.Join(x.root, "Movie/movie.mkv")); err != nil {
		t.Fatal(err)
	}
	review = mediaRemovalTestReview(t, x, v.ID)
	if err := x.s.ReturnMediaRemovalCapture(t.Context(), review, 0, func(RecoveryScope) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if x.bytes(t, "Movie/movie.mkv") != "original" {
		t.Fatal("return changed content")
	}
	old, err := x.s.Get(t.Context(), job)
	if err != nil || old.State != RecoveryClosed {
		t.Fatal(old.State, err)
	}
	closed, _ := x.s.MediaRemoval(t.Context(), v.ID)
	if closed.State != "closed" {
		t.Fatal(closed.State)
	}
}
func TestMediaRemovalClaimsBlockFreshWritersAndPublicPrivateRecoveryCannotBypassTheParent(t *testing.T) {
	x := recoveryFixture(t, "")
	x.put(t, "Movie/movie.mkv", "movie")
	v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "file", 0)}, nil))
	if _, err := x.s.CreateRecoveryDisposal(t.Context(), v.Definition.Units[0].Definition, Digest(v.Definition.Units[0].Definition)); !errors.Is(err, ErrState) {
		t.Fatal("private endpoint granted ordinary media", err)
	}
	info, _ := os.Stat(filepath.Join(x.root, "Movie/movie.mkv"))
	d := Definition{SourceRoot: x.root, TargetRoot: x.root, SourceIdentity: x.scope.Root.Identity, TargetIdentity: x.scope.Root.Identity, SourceID: "root", TargetID: "root", Mode: "copy", Entries: []Entry{{Source: "Movie/movie.mkv", Target: "other.mkv", Kind: "media", Size: info.Size(), Identity: Identity(info)}}}
	if _, err := x.s.Create(t.Context(), Digest(d), d); !errors.Is(err, ErrClaimed) {
		t.Fatal("fresh writer bypassed removal", err)
	}
	if err := x.s.CloseMediaRemoval(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Create(t.Context(), Digest(d), d); !errors.Is(err, ErrClaimed) {
		t.Fatal("stop released unresolved scope", err)
	}
}

func TestMediaRemovalOrdinaryWholeDiscDirectoryAndKnownAbsenceAcrossProviders(t *testing.T) {
	for _, driver := range []string{"", "rclone", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryFixture(t, driver)
			mediaRemovalHistoryFixture(t, x, 1)
			x.put(t, "Movie/BDMV/index.bdmv", "disc index")
			x.put(t, "Movie/BDMV/STREAM/00001.m2ts", "complete payload")
			x.put(t, "Movie/unknown.txt", "explicitly reviewed unknown")
			x.put(t, "outside/keep.mkv", "outside")
			if driver == "" {
				if err := os.Link(filepath.Join(x.root, "outside/keep.mkv"), filepath.Join(x.root, "Movie/hardlink")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(x.root, "outside/keep.mkv"), filepath.Join(x.root, "Movie/link")); err != nil {
					t.Fatal(err)
				}
			}
			v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie", "library", 1)}, []int64{1}))
			if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err != nil {
				t.Fatal(err)
			}
			if x.exists(t, "Movie") || x.bytes(t, "outside/keep.mkv") != "outside" {
				t.Fatal("directory removal changed outside data")
			}
			mediaRemovalHistoryFixture(t, x, 2)
			v = mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Missing/already-gone.mkv", "library", 2)}, []int64{2}))
			if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err != nil {
				t.Fatal(err)
			}
			events, err := x.s.BusinessEvents().List(t.Context())
			if err != nil || len(events) != 1 {
				t.Fatal("known absence forged a second event", events, err)
			}
		})
	}
}

func TestMediaRemovalFreezesWholeLegacyBatchBeforeCaptureAndRetainsEveryOldTargetClaim(t *testing.T) {
	x := recoveryFixture(t, "")
	mediaRemovalHistoryFixture(t, x, 1)
	x.put(t, "Movie/movie.mkv", "selected")
	x.put(t, "unrelated/source.mkv", "unselected sibling source")
	batch := strings.Repeat("c", 64)
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	doc := legacyBatchDocument{Jobs: ids, Definitions: []Definition{}}
	for i, id := range ids {
		source := "Movie/movie.mkv"
		if i == 1 {
			source = "unrelated/source.mkv"
		}
		info, _ := os.Stat(filepath.Join(x.root, source))
		d := Definition{SourceRoot: x.root, TargetRoot: x.root, SourceIdentity: x.scope.Root.Identity, TargetIdentity: x.scope.Root.Identity, SourceID: "root", TargetID: "root", Mode: "copy", LegacyBatch: &LegacyBatchRef{ID: batch, Index: i}, Entries: []Entry{{Source: source, Target: "published/" + strconv.Itoa(i) + ".mkv", Kind: "media", Size: info.Size(), Identity: Identity(info)}}}
		doc.Definitions = append(doc.Definitions, d)
		raw, _ := json.Marshal(d)
		if _, err := x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_JOBS VALUES(?,?,?,'fixture','active')`, id, Digest(d), string(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_ITEMS(JOB_ID,ORDINAL,STATE)VALUES(?,0,'planned')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_TARGETS VALUES(?,?)`, Digest(d.TargetIdentity+":"+d.Entries[0].Target), id); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(doc)
	if _, err := x.s.db.Exec(`INSERT INTO GO_LEGACY_ORGANIZATION_BATCHES VALUES(?,?,?,'ready','','admin','fixture')`, batch, string(raw), Digest(doc)); err != nil {
		t.Fatal(err)
	}
	v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1}))
	if len(v.Owners) != 2 || !v.Owners[1].Related {
		t.Fatal(v.Owners)
	}
	v = mediaRemovalTestReview(t, x, v.ID)
	if err := x.s.ExecuteMediaRemoval(t.Context(), v, []*remotestore.Client{nil}, v.Fingerprint, func(RecoveryScope) error {
		var state string
		_ = x.s.db.QueryRow(`SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, v.ID).Scan(&state)
		if state == "running" {
			var n int
			_ = x.s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS WHERE STATE=?`, RecoveryClosed).Scan(&n)
			if n != 2 {
				t.Fatal("first intent did not close all siblings", n)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = x.s.db.QueryRow(`SELECT STATE FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE ID=?`, batch).Scan(&state)
	if state != "closing" {
		t.Fatal(state)
	}
	var claims int
	_ = x.s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS`).Scan(&claims)
	if claims != 2 {
		t.Fatal("disposal silently released target claims", claims)
	}
	if x.bytes(t, "unrelated/source.mkv") != "unselected sibling source" {
		t.Fatal("closed sibling's source changed")
	}
}

func TestMediaRemovalChildCompletionAndTypedEventCommitAtomically(t *testing.T) {
	x := recoveryFixture(t, "minio")
	mediaRemovalHistoryFixture(t, x, 1)
	x.put(t, "Movie/movie.mkv", "movie")
	v := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1}))
	if _, err := x.s.db.Exec(`CREATE TRIGGER refuse_unit_receipt BEFORE UPDATE OF STATE ON GO_MEDIA_REMOVAL_UNITS WHEN NEW.STATE='done' BEGIN SELECT RAISE(ABORT,'fixture atomic receipt refusal');END`); err != nil {
		t.Fatal(err)
	}
	if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err == nil {
		t.Fatal("expected atomic receipt refusal")
	}
	child, err := x.s.RecoveryDisposal(t.Context(), mediaRemovalChildID(v.Definition, 0))
	if err != nil || child.State == "completed" {
		t.Fatal("child committed without its typed event", child.State, err)
	}
	events, err := x.s.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 0 {
		t.Fatal("event escaped rolled-back completion", events, err)
	}
	if _, err := x.s.db.Exec(`DROP TRIGGER refuse_unit_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := mediaRemovalTestExecute(t, x, mediaRemovalTestReview(t, x, v.ID)); err != nil {
		t.Fatal(err)
	}
	events, err = x.s.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	deletes := 0
	for _, r := range x.cloud.Requests() {
		if r.Method == "DELETE" && r.Key == "bucket/library/Movie/movie.mkv" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatal("atomic receipt reconciliation replayed deletion", deletes)
	}
}

func TestMediaRemovalRawRowBudgetIsAggregateNotPerHistoricalID(t *testing.T) {
	used := (64 << 20) - 1
	row := MediaRemovalRow{Table: "TRANSFER_HISTORY", Columns: []string{"ID"}, Values: []MediaRemovalValue{{Kind: "integer", Int: 1}}}
	if err := mediaRemovalRowBudget([]MediaRemovalRow{row}, &used); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	used = 0
	if err := mediaRemovalRowBudget([]MediaRemovalRow{row}, &used); err != nil || used <= 0 {
		t.Fatal(used, err)
	}
}
