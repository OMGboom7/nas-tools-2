//go:build linux || darwin

package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/movielike"
)

type favoriteUnit struct {
	x  *recoveryUnit
	id string
	c  movielike.Control
}

func favoriteFixture(t *testing.T) *favoriteUnit {
	t.Helper()
	x := recoveryFixture(t, "")
	if _, err := x.s.db.Exec(`CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT);
 INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','UserInstalledPlugins','["MovieLike"]'),('SystemConfig','plugin.MovieLike','{"enable":true,"dir_name":"精选"}');
 CREATE TABLE TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,DEST_PATH TEXT,EXTRA BLOB,NOTE TEXT);
 INSERT INTO TRANSFER_HISTORY VALUES(9007199254740993,'unchanged original path',X'00FF01',NULL);`); err != nil {
		t.Fatal(err)
	}
	raw, _ := movielike.Setting(t.Context(), x.s.db, movielike.ConfigKey)
	spec, _ := movielike.ParseSpec(raw)
	at := movielike.Now()
	basis, _ := x.s.MovieLike().Basis(t.Context())
	c := movielike.Control{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: Digest("admin"), AppDigest: x.scope.ConfigDigest, CategoryDigest: Digest("categories"), ConfigDigest: movielike.Hash(raw), MediaID: "original-emby", Created: at, Categories: []string{"Films"}, Roots: []movielike.Root{{ID: x.scope.Root.ID, Path: x.root, Identity: x.scope.Root.Identity, ConfiguredPath: x.root}}}
	p := movielike.Review{ID: strings.Repeat("b", 32), Basis: basis, Created: at, Control: c, Spec: spec}
	allow := func() error { return nil }
	if err := x.s.MovieLike().SaveReview(t.Context(), p, allow); err != nil {
		t.Fatal(err)
	}
	if err := x.s.MovieLike().Activate(t.Context(), p, allow); err != nil {
		t.Fatal(err)
	}
	if err := x.s.MovieLike().Transaction(t.Context(), func(tx *sql.Tx) error {
		return movielike.Capture(t.Context(), tx, "emby", strings.Repeat("c", 32), json.RawMessage(`{"Event":"item.rate","Item":{"Type":"Movie","Name":"Original","Path":"/emby/Films/Movie/movie.mkv"}}`))
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := x.s.MovieLike().List(t.Context(), 1, true)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	for name, body := range map[string]string{"Films/Movie/movie.mkv": "original media", "Films/Movie/movie.nfo": "original metadata", "Films/Movie/extras/readme.txt": "original extra", "keep.txt": "must remain"} {
		x.put(t, name, body)
	}
	if err := os.MkdirAll(path.Join(x.root, "Films/Movie/empty"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("movie.mkv", path.Join(x.root, "Films/Movie/shortcut.mkv")); err != nil {
		t.Fatal(err)
	}
	current, _, _, err := x.s.MovieLike().Scope(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return &favoriteUnit{x, rows[0].ID, current}
}
func (f *favoriteUnit) save(t *testing.T, aliases ...string) FavoriteMoveReview {
	t.Helper()
	p, err := f.x.s.MovieLike().Receipt(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.x.s.PrepareFavoriteMove(t.Context(), p, f.x.scope, "Films/Movie", "精选/Movie", aliases...)
	if err != nil {
		t.Fatal(err)
	}
	if v.Files != 3 || v.Links != 1 || v.Directories != 3 {
		t.Fatal("whole ordinary directory was not captured", v)
	}
	if err := f.x.s.SaveFavoriteMove(t.Context(), v, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	v, err = f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || !v.CanContinue {
		t.Fatal(v.Evidence, err)
	}
	return v
}
func (f *favoriteUnit) copyBranch(t *testing.T, v FavoriteMoveReview) FavoriteMoveReview {
	t.Helper()
	if err := os.MkdirAll(path.Join(f.x.root, "精选"), 0755); err != nil {
		t.Fatal(err)
	}
	source, _, err := favoriteParent(v.Definition, v.Definition.Source, false)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := source.Stat()
	source.Close()
	target, _, err := favoriteParent(v.Definition, v.Definition.Target, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := target.Stat()
	target.Close()
	proof := FavoriteMoveProof{Method: "copy", SourceParent: Identity(a), TargetParent: Identity(b)}
	if err := f.x.s.favoriteSave(t.Context(), f.id, "planned", "copying", "exercise cross-filesystem copy algorithm", "", proof, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	out, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || !out.CanContinue {
		t.Fatal(out.Evidence, err)
	}
	return out
}
func favoriteExecuteTest(t *testing.T, f *favoriteUnit, v FavoriteMoveReview, automatic bool) error {
	t.Helper()
	return f.x.s.ExecuteFavoriteMove(t.Context(), v, "admin", automatic, func() error { return nil })
}
func favoriteAssertComplete(t *testing.T, f *favoriteUnit) {
	t.Helper()
	p, err := f.x.s.MovieLike().Receipt(t.Context(), f.id)
	if err != nil || p.State != "completed" {
		t.Fatal(p, err)
	}
	if f.x.exists(t, "Films/Movie") || !f.x.exists(t, "精选/Movie/empty") || f.x.bytes(t, "精选/Movie/extras/readme.txt") != "original extra" || f.x.bytes(t, "keep.txt") != "must remain" {
		t.Fatal("whole move lost or expanded content")
	}
	link, err := os.Readlink(path.Join(f.x.root, "精选/Movie/shortcut.mkv"))
	if err != nil || link != "movie.mkv" {
		t.Fatal(link, err)
	}
	var id int64
	var dest, extra string
	var note sql.NullString
	if err := f.x.s.db.QueryRow(`SELECT ID,DEST_PATH,hex(EXTRA),NOTE FROM TRANSFER_HISTORY`).Scan(&id, &dest, &extra, &note); err != nil || id != 9007199254740993 || dest != "unchanged original path" || extra != "00FF01" || note.Valid {
		t.Fatal("original history changed", id, dest, extra, note, err)
	}
	rows, err := f.x.s.MovieLike().List(t.Context(), 1, true)
	if err != nil || len(rows) != 0 {
		t.Fatal("completed work returned to pending", rows, err)
	}
	v, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || v.CanContinue {
		t.Fatal(v, err)
	}
	if err := favoriteExecuteTest(t, f, v, false); err == nil {
		t.Fatal("completed original replayed")
	}
}
func TestFavoriteCompleteDirectoryRenameAndVerifiedCopyKeepHistoryAndLinks(t *testing.T) {
	for _, method := range []string{"rename", "copy"} {
		t.Run(method, func(t *testing.T) {
			f := favoriteFixture(t)
			v := f.save(t)
			if method == "copy" {
				v = f.copyBranch(t, v)
			}
			if err := favoriteExecuteTest(t, f, v, true); err != nil {
				t.Fatal(err)
			}
			favoriteAssertComplete(t, f)
		})
	}
}
func TestFavoriteNoOverwriteAndChangedSourceOrParentNeverMoveSubset(t *testing.T) {
	for _, change := range []string{"target", "bytes", "new-child", "parent", "symlink"} {
		t.Run(change, func(t *testing.T) {
			f := favoriteFixture(t)
			v := f.save(t)
			switch change {
			case "target":
				f.x.put(t, "精选/Movie/foreign.txt", "must remain")
			case "bytes":
				f.x.put(t, "Films/Movie/movie.mkv", "changed original bytes")
			case "new-child":
				f.x.put(t, "Films/Movie/new.txt", "new member")
			case "parent":
				old := path.Join(f.x.root, "Films")
				if err := os.Rename(old, old+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(old, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(old+".retained/Movie", old+"/Movie"); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path.Join(f.x.root, "Films/Movie"), path.Join(f.x.root, "retained")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path.Join(f.x.root, "retained"), path.Join(f.x.root, "Films/Movie")); err != nil {
					t.Fatal(err)
				}
			}
			current, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if current.CanContinue {
				t.Fatal("changed original can continue", change)
			}
			if err := favoriteExecuteTest(t, f, v, true); err == nil {
				t.Fatal("old review adopted changed scope")
			}
			if change == "target" && f.x.bytes(t, "精选/Movie/foreign.txt") != "must remain" {
				t.Fatal("target overwritten")
			}
		})
	}
}
func TestFavoriteFinalCommitFailureKeepsMovedFactAndOnlyExplicitReviewReceiptsIt(t *testing.T) {
	f := favoriteFixture(t)
	v := f.save(t)
	if _, err := f.x.s.db.Exec(`CREATE TRIGGER fail_favorite_final BEFORE UPDATE ON GO_MOVIE_LIKE_EVENTS WHEN NEW.STATE='completed' BEGIN SELECT RAISE(ABORT,'fixture failure');END`); err != nil {
		t.Fatal(err)
	}
	if err := favoriteExecuteTest(t, f, v, true); err == nil {
		t.Fatal("lost final transaction claimed complete")
	}
	p, err := f.x.s.MovieLike().Receipt(t.Context(), f.id)
	if err != nil || p.State != "needs_review" || p.Phase != "moved" || f.x.exists(t, "Films/Movie") || !f.x.exists(t, "精选/Movie/movie.mkv") {
		t.Fatal(p, err)
	}
	if rows, _ := f.x.s.MovieLike().List(t.Context(), 1, true); len(rows) != 0 {
		t.Fatal("restart would replay moved original")
	}
	if _, err := f.x.s.db.Exec("DROP TRIGGER fail_favorite_final"); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || !fresh.CanContinue {
		t.Fatal(fresh, err)
	}
	if err := favoriteExecuteTest(t, f, fresh, false); err != nil {
		t.Fatal(err)
	}
	favoriteAssertComplete(t, f)
}
func TestFavoriteCopiedSourcePartialCleanupStopsAndResumesOnlyRemainingOriginal(t *testing.T) {
	f := favoriteFixture(t)
	v := f.copyBranch(t, f.save(t))
	stopped := f.c
	stopped.Authorized = false
	raw, _ := json.Marshal(stopped)
	trigger := `CREATE TRIGGER stop_favorite_cleanup AFTER UPDATE ON GO_MOVIE_LIKE_EVENTS WHEN NEW.STATE='running' AND NEW.PHASE='cleaning' AND json_array_length(json_extract(NEW.PROOF,'$.Deleted'))=1 BEGIN UPDATE SYSTEM_DICT SET VALUE='` + strings.ReplaceAll(string(raw), "'", "''") + `' WHERE KEY='GoMovieLike';END`
	if _, err := f.x.s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if err := favoriteExecuteTest(t, f, v, true); err == nil {
		t.Fatal("revoked authority cleaned remaining source")
	}
	p, err := f.x.s.MovieLike().Receipt(t.Context(), f.id)
	if err != nil || p.State != "needs_review" || p.Phase != "cleaning" {
		t.Fatal(p, err)
	}
	d, proof, err := favoriteWork(p)
	if err != nil || len(proof.Deleted) != 1 || !f.x.exists(t, favoriteCapture(d)) || !f.x.exists(t, "精选/Movie/movie.mkv") {
		t.Fatal(proof, err)
	}
	if rows, _ := f.x.s.MovieLike().List(t.Context(), 1, true); len(rows) != 0 {
		t.Fatal("partial cleanup automatically requeued")
	}
	fresh, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || !fresh.CanContinue {
		t.Fatal(fresh.Evidence, err)
	}
	if err := favoriteExecuteTest(t, f, fresh, false); err != nil {
		t.Fatal(err)
	}
	favoriteAssertComplete(t, f)
	if f.x.exists(t, favoriteCapture(d)) || f.x.exists(t, favoriteStage(d)) {
		t.Fatal("owned recovery objects remained after full cleanup")
	}
}
func TestFavoriteUnprovedStageAndCompetingAliasClaimsAreNeverAdopted(t *testing.T) {
	f := favoriteFixture(t)
	alias := path.Join(t.TempDir(), "library-alias")
	if err := os.Symlink(f.x.root, alias); err != nil {
		t.Fatal(err)
	}
	v := f.save(t, alias)
	if err := favoriteClaims(t.Context(), f.x.s.db, []string{path.Join(alias, "Films/Movie/movie.mkv")}, ""); !errors.Is(err, ErrClaimed) {
		t.Fatal("saved physical-root alias not protected", err)
	}
	if err := mediaRemovalClaimed(t.Context(), f.x.s.db, []string{path.Join(alias, "精选/Movie")}, nil, ""); !errors.Is(err, ErrClaimed) {
		t.Fatal(err)
	}
	v = f.copyBranch(t, v)
	stage := favoriteStage(v.Definition)
	f.x.put(t, path.Join(stage, "foreign.txt"), "never adopt")
	fresh, err := f.x.s.ReviewFavoriteMove(t.Context(), f.id)
	if err != nil || fresh.CanContinue {
		t.Fatal(fresh, err)
	}
	if err := favoriteExecuteTest(t, f, v, true); err == nil {
		t.Fatal("foreign private name was adopted")
	}
	if f.x.bytes(t, path.Join(stage, "foreign.txt")) != "never adopt" || !f.x.exists(t, "Films/Movie/movie.mkv") {
		t.Fatal("foreign stage or source changed")
	}
}
func TestFavoriteIgnoreOnlyStopsAndProtectsIncompleteObjects(t *testing.T) {
	f := favoriteFixture(t)
	v := f.save(t)
	if err := f.x.s.IgnoreFavoriteMove(context.Background(), f.id, "admin", "fixture stopped", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p, err := f.x.s.MovieLike().Receipt(t.Context(), f.id)
	if err != nil || p.State != "ignored" {
		t.Fatal(p, err)
	}
	_, _, on, err := f.x.s.MovieLike().Scope(t.Context())
	if err != nil || on {
		t.Fatal(on, err)
	}
	if !f.x.exists(t, "Films/Movie/movie.mkv") {
		t.Fatal("stop moved files")
	}
	if err := favoriteClaims(t.Context(), f.x.s.db, favoriteLocations(v.Definition), ""); !errors.Is(err, ErrClaimed) {
		t.Fatal("stop silently released original scope", err)
	}
}
func TestFavoriteStopsAnExistingWriterWithoutFabricatingItsItemSuccess(t *testing.T) {
	f := favoriteFixture(t)
	id := Digest("older organization writer")[:32]
	definition := Definition{SourceRoot: "/offline-original-input", TargetRoot: f.x.root, TargetIdentity: f.x.scope.Root.Identity, SourceIdentity: "offline", ConfigDigest: f.x.scope.ConfigDigest, Mode: "copy", Entries: []Entry{{Source: "original.mkv", Target: "Films/Movie/movie.mkv", Kind: "media", Title: "Original", MediaType: "电影"}}}
	raw, _ := json.Marshal(definition)
	if _, err := f.x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_JOBS(ID,FINGERPRINT,DEFINITION,CREATED,STATE)VALUES(?,?,?,?,'active')`, id, Digest(definition), string(raw), movielike.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_ITEMS(JOB_ID,ORDINAL,STATE,REASON,PROOF)VALUES(?,0,'pending','','{}')`, id); err != nil {
		t.Fatal(err)
	}
	v := f.save(t)
	if len(v.Owners) != 1 || v.Owners[0].ID != id {
		t.Fatal("old writer omitted from whole scope", v.Owners)
	}
	if err := favoriteExecuteTest(t, f, v, true); err != nil {
		t.Fatal(err)
	}
	var state, item string
	if err := f.x.s.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := f.x.s.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, id).Scan(&item); err != nil {
		t.Fatal(err)
	}
	if state != RecoveryClosed || item != "pending" {
		t.Fatal("old authority was not closed or success invented", state, item)
	}
	favoriteAssertComplete(t, f)
}
