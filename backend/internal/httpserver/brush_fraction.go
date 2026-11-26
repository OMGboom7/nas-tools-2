package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/diskusage"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

const brushFractionSchema = `
CREATE TABLE IF NOT EXISTS GO_BRUSH_FRACTION(token TEXT PRIMARY KEY, config_digest TEXT NOT NULL, proof TEXT NOT NULL, size INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS go_brush_fraction_finished AFTER DELETE ON GO_BRUSH_SUBMISSION BEGIN
 DELETE FROM GO_BRUSH_FRACTION WHERE token=OLD.token; END;`

type brushSubmitRequest struct {
	Brush                                                         brushDownloadRequest
	Media                                                         downloadhistory.Media
	SavePath                                                      *string
	Rule                                                          brushFractionRule
	RuleSource, ClientType, Directory, LocalDir, Category, Magnet string
	Config, Directories                                           json.RawMessage
	Tags                                                          []string
	Torrent                                                       []byte
	UploadLimitKB, DownloadLimitKB, SeedingTimeLimit              int
	RatioLimit                                                    float64
}
type brushSubmitResult struct {
	Pending     bool
	Token, Task string
	Size        int64
}

func BrushSubmitCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	reader := &io.LimitedReader{R: in, N: 16 << 20}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var q brushSubmitRequest
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || reader.N == 0 || op != "brush-whole" && op != "brush-fraction" || q.Brush.Partial != (op == "brush-fraction") {
		return errBrushFraction
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	db, err := (subscriptionService{databasePath: dbPath}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	s := downloadService{client: http.DefaultClient, downloaders: store}
	if organization.CopySupported {
		locks, err := organization.OpenStore(dbPath)
		if err != nil {
			return err
		}
		defer locks.Close()
		guard, err := locks.AcquireJob(ctx, "brush-selection:"+strconv.FormatInt(q.Brush.TaskID, 10))
		if err != nil {
			return err
		}
		defer guard.Close()
		s.manualGuards = &locks
	}
	ctx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	id, err := strconv.ParseInt(q.Brush.Downloader, 10, 64)
	if err != nil || (q.Torrent == nil) == (q.Magnet == "") {
		return errBrushDetail
	}
	item, err := store.Get(ctx, id)
	if err != nil || item.Enabled == 0 || item.Type != q.ClientType || !sameClassicJSON(item.Config, q.Config) || !sameClassicJSON(item.DownloadDir, q.Directories) {
		return errBrushDetail
	}
	var result brushSubmitResult
	if q.Brush.Partial {
		result, err = s.submitBrushFraction(ctx, db, item, q)
	} else {
		result, err = s.submitBrushWhole(ctx, db, item, q)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

type brushBudgetReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func brushFractionBudget(ctx context.Context, db brushBudgetReader, q brushSubmitRequest) (int64, error) {
	info, err := os.Stat(q.LocalDir)
	if err != nil || !info.IsDir() {
		return 0, errBrushFraction
	}
	space, err := diskusage.Read(q.LocalDir)
	if err != nil {
		return 0, err
	}
	var seed string
	var active, pending int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(SEED_SIZE,''),
	(SELECT COALESCE(SUM(CAST(TORRENT_SIZE AS INTEGER)),0) FROM SITE_BRUSH_TORRENTS WHERE TASK_ID=t.ID AND DOWNLOAD_ID!='0'),
	(SELECT COALESCE(SUM(COALESCE(f.size,s.size)),0) FROM GO_BRUSH_SUBMISSION s LEFT JOIN GO_BRUSH_FRACTION f ON f.token=s.token
	WHERE s.task_id=t.ID AND (s.ready=1 OR f.token IS NOT NULL))
	FROM SITE_BRUSH_TASK t WHERE t.ID=? AND STATE='Y' AND COALESCE(FRACTION_RULE,'')=?`, q.Brush.TaskID, q.RuleSource).Scan(&seed, &active, &pending)
	if err != nil {
		return 0, err
	}
	remaining := q.Brush.Size
	if seed != "" {
		gb, e := strconv.ParseFloat(seed, 64)
		n := gb * (1 << 30)
		if e != nil || math.IsNaN(n) || n < 0 || n >= float64(math.MaxInt64) || active < 0 || pending < 0 || int64(n)-active <= pending {
			return 0, errBrushFraction
		}
		remaining = int64(n) - active - pending
	}
	return q.Rule.limit(q.Brush.Size, remaining, int64(min(space.AvailableBytes, float64(1<<62))))
}

func (s downloadService) submitBrushFraction(ctx context.Context, db *sql.DB, item downloaderconfig.Downloader, q brushSubmitRequest) (brushSubmitResult, error) {
	var result brushSubmitResult
	if item.Type != "transmission" && item.Type != "qbittorrent" {
		return result, errBrushFraction
	}
	if _, err := db.ExecContext(ctx, brushReceiptSchema+brushFractionSchema); err != nil {
		return result, err
	}
	limit, err := brushFractionBudget(ctx, db, q)
	if err != nil {
		return result, err
	}
	var manifest []torrentfiles.File
	if q.Torrent != nil {
		_, manifest, err = torrentmeta.Manifest(q.Torrent)
		if err != nil {
			return result, err
		}
		if _, err = planBrushFiles(manifest, limit); err != nil {
			return result, err
		}
	}
	identity, err := q.contentIdentity(item.Type)
	if err != nil {
		return result, err
	}
	q.Brush.FractionSource = &q.RuleSource
	token, err := reserveBrushDownload(ctx, db, q.Brush, nil)
	if err != nil || token == "" {
		return result, err
	}
	hash, handled, err := s.addBrushDownload(ctx, item, q, token, identity)
	if err != nil || !handled || hash == "" {
		return result, errBrushFraction
	}
	err = acceptBrushDownload(ctx, db, token, hash)
	if err != nil {
		return result, err
	}
	snapshot := func() (torrentfiles.Snapshot, error) {
		var active int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_BRUSH_SUBMISSION s JOIN SITE_BRUSH_TASK t ON t.ID=s.task_id WHERE token=? AND remote_id=? AND t.STATE='Y' AND COALESCE(t.FRACTION_RULE,'')=? AND COALESCE(t.RSS_RULE,'')=?`, token, hash, q.RuleSource, q.Brush.RuleSource).Scan(&active)
		if err != nil || active != 1 {
			return torrentfiles.Snapshot{}, errBrushFraction
		}
		st, err := s.brushRemote(ctx, item, hash, token)
		if err != nil || q.Directory != "" && filepath.Clean(st.Directory) != filepath.Clean(q.Directory) {
			return st, errBrushFraction
		}
		return st, nil
	}
	st, err := awaitBrushSnapshot(ctx, snapshot, func(st torrentfiles.Snapshot) bool { return st.Paused })
	if err != nil || !st.Paused || manifest != nil && !torrentfiles.MatchTransfer(manifest, st.Files) {
		return result, errBrushFraction
	}
	limit, err = brushFractionBudget(ctx, db, q)
	if err != nil {
		return result, err
	}
	padding := map[string]bool{}
	for _, f := range manifest {
		if f.Padding {
			padding[f.Path] = true
		}
	}
	for i, f := range st.Files {
		if padding[f.Path] {
			st.Files[i].Padding = true
			st.Files[i].Selected = false
		}
	}
	plan, err := planBrushFiles(st.Files, limit)
	if err != nil {
		return result, err
	}
	proof, err := s.applyBrushPriorities(ctx, item, hash, plan, snapshot)
	if err != nil {
		return result, err
	}
	if err := reserveBrushStart(ctx, db, q, token, hash, proof, bus.Hash(item), plan.Size); err != nil {
		return result, err
	}
	if item.Type == "qbittorrent" {
		c, e := s.qbProfileClient(item)
		if e != nil {
			return result, e
		}
		err = c.StartSelected(ctx, hash)
	} else {
		c, e := s.profileClient(item)
		if e != nil {
			return result, e
		}
		err = c.Control(ctx, hash, "start")
	}
	if err != nil {
		return result, err
	}
	st, err = awaitBrushSnapshot(ctx, snapshot, brushFractionRunning)
	if err != nil || !brushFractionRunning(st) || brushFractionProof(st) != proof {
		return result, errBrushFraction
	}
	// A verified remote start is still a successful download if its final
	// local write failed. Preserve the ordinary download history as well.
	err = markBrushFractionReady(ctx, db, token, hash, proof)
	return brushSubmitResult{Token: token, Task: hash, Size: plan.Size, Pending: err != nil}, nil
}

func (s downloadService) applyBrushPriorities(ctx context.Context, item downloaderconfig.Downloader, hash string, plan brushFilePlan, snapshot func() (torrentfiles.Snapshot, error)) (string, error) {
	if item.Type == "qbittorrent" {
		c, e := s.qbProfileClient(item)
		if e != nil {
			return "", e
		}
		for _, level := range []int{0, 1, 6, 7} {
			var ids []int
			for _, f := range plan.Files {
				if plan.Priorities[f.Index] == level {
					ids = append(ids, f.Index)
				}
			}
			if len(ids) == 0 {
				continue
			}
			current, e := snapshot()
			if e != nil || !current.Paused || !brushSameFiles(plan.Files, current.Files) {
				return "", errBrushFraction
			}
			if e = c.SetPriority(ctx, hash, ids, level); e != nil {
				return "", e
			}
		}
	} else {
		c, e := s.profileClient(item)
		if e != nil {
			return "", e
		}
		pri := map[int][]int{}
		for _, f := range plan.Files {
			level := map[int]int{0: -1, 1: -1, 6: 0, 7: 1}[plan.Priorities[f.Index]]
			pri[level] = append(pri[level], f.Index)
		}
		if e = c.SetPriorities(ctx, hash, torrentfiles.IDs(plan.Files, true), torrentfiles.IDs(plan.Files, false), pri); e != nil {
			return "", e
		}
	}
	st, err := snapshot()
	if err != nil || !st.Paused || !brushSameFiles(plan.Files, st.Files) {
		return "", errBrushFraction
	}
	for _, f := range st.Files {
		level := plan.Priorities[f.Index]
		if item.Type == "transmission" {
			level = map[int]int{0: -1, 1: -1, 6: 0, 7: 1}[level]
		}
		if p, ok := st.Priorities[f.Index]; !ok || p != level || f.Selected != (plan.Priorities[f.Index] != 0) {
			return "", errBrushFraction
		}
	}
	return brushFractionProof(st), nil
}

// Reserve bytes under SQLite's write lock before starting, including on
// platforms without filesystem job locks. A second plan must still fit next
// to every outstanding start even when it computed its selection earlier.
func reserveBrushStart(ctx context.Context, db *sql.DB, q brushSubmitRequest, token, hash, proof, digest string, size int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	limit, err := brushFractionBudget(ctx, tx, q)
	if err != nil {
		return err
	}
	if size <= 0 || size > limit {
		return errBrushFraction
	}
	err = fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_BRUSH_FRACTION(token,config_digest,proof,size)
	SELECT token,?,?,? FROM GO_BRUSH_SUBMISSION s JOIN SITE_BRUSH_TASK t ON t.ID=s.task_id
	WHERE token=? AND task_id=? AND remote_id=? AND partial=1 AND ready=0 AND t.STATE='Y' AND COALESCE(t.RSS_RULE,'')=?`,
		digest, proof, size, token, q.Brush.TaskID, hash, q.Brush.RuleSource))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// A successful add/start acknowledgement can precede observable metadata or state.
func awaitBrushSnapshot(ctx context.Context, read func() (torrentfiles.Snapshot, error), ready func(torrentfiles.Snapshot) bool) (torrentfiles.Snapshot, error) {
	wait, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		st, err := read()
		if err == nil && ready(st) {
			return st, nil
		}
		select {
		case <-wait.Done():
			return st, errBrushFraction
		case <-ticker.C:
		}
	}
}
func brushSameFiles(a, b []torrentfiles.File) bool {
	return slices.EqualFunc(a, b, func(x, y torrentfiles.File) bool { return x.Index == y.Index && x.Path == y.Path && x.Size == y.Size })
}

func brushMarker(token string) string { return "NASTOOL-BRUSH-" + bus.Hash(token)[:24] }

func (s downloadService) brushRemote(ctx context.Context, item downloaderconfig.Downloader, hash, token string) (torrentfiles.Snapshot, error) {
	var st torrentfiles.Snapshot
	current, err := s.downloaders.Get(ctx, item.ID)
	if err != nil || bus.Hash(item) != bus.Hash(current) || current.Enabled == 0 {
		return st, errBrushFraction
	}
	switch item.Type {
	case "qbittorrent":
		c, e := s.qbProfileClient(item)
		if e != nil {
			return st, e
		}
		st, err = c.Selection(ctx, hash)
	case "transmission":
		c, e := s.profileClient(item)
		if e != nil {
			return st, e
		}
		st, err = c.Selection(ctx, hash)
	default:
		return st, errBrushFraction
	}
	if err != nil || !strings.EqualFold(st.Hash, hash) || !slices.Contains(st.Labels, brushMarker(token)) || len(st.Priorities) != len(st.Files) {
		return st, errBrushFraction
	}
	return st, nil
}
func brushFractionRunning(st torrentfiles.Snapshot) bool {
	if st.Paused {
		return false
	}
	switch st.State {
	case "downloading", "stalledDL", "queuedDL", "uploading", "stalledUP", "queuedUP", "download-wait", "seed-wait", "seeding":
		return true
	}
	return false
}
func brushFractionProof(st torrentfiles.Snapshot) string {
	return bus.Hash([]any{st.Hash, st.Directory, st.Files, st.Priorities})
}
func markBrushFractionReady(ctx context.Context, db *sql.DB, token, hash, proof string) error {
	return fuzzyOne(db.ExecContext(ctx, `UPDATE GO_BRUSH_SUBMISSION SET ready=1,size=(SELECT size FROM GO_BRUSH_FRACTION WHERE token=?) WHERE token=? AND remote_id=? AND partial=1 AND task_id IS NOT NULL AND EXISTS(SELECT 1 FROM GO_BRUSH_FRACTION WHERE token=? AND proof=?)`, token, token, hash, token, proof))
}

// Recovery only observes previously issued starts. A missing acknowledgement
// must never cause another add, file selection, start or removal.
func recoverBrushFractions(ctx context.Context, db *sql.DB, dbPath string, task int64) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_BRUSH_FRACTION' AND type='table')`).Scan(&exists); err != nil || !exists {
		return err
	}
	type pending struct {
		token, hash, config, proof string
		id                         int64
	}
	rows, err := db.QueryContext(ctx, `SELECT s.token,s.remote_id,f.config_digest,f.proof,s.downloader FROM GO_BRUSH_SUBMISSION s JOIN GO_BRUSH_FRACTION f USING(token) WHERE s.ready=0 AND s.task_id IS NOT NULL AND (?=0 OR s.task_id=?)`, task, task)
	if err != nil {
		return err
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.token, &p.hash, &p.config, &p.proof, &p.id); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(list) == 0 {
		return err
	}
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	s := downloadService{client: http.DefaultClient, downloaders: store}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, p := range list {
		item, e := store.Get(ctx, p.id)
		if e != nil || bus.Hash(item) != p.config {
			continue
		}
		st, e := s.brushRemote(ctx, item, p.hash, p.token)
		if e == nil && brushFractionRunning(st) && brushFractionProof(st) == p.proof {
			if e = markBrushFractionReady(ctx, db, p.token, p.hash, p.proof); e != nil && !errors.Is(e, errFuzzyScope) {
				return e
			}
		}
	}
	return nil
}
