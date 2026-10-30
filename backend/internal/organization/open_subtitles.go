package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
)

type OpenSubtitleDefinition struct {
	Version                                           int
	Kind, Reference, Origin, File, Extension, Created string
	Ordinal                                           int
	Bluray                                            bool
	RemoteWrite                                       bool
	Media                                             map[string]any
	Provider                                          opensubtitles.Snapshot
	Source                                            *bus.EventWebhookSource `json:",omitempty"`
}

// OpenSubtitleLibrary binds the existing metadata publisher/journal to the
// original subtitle consumer. Generic NFO workers must never execute this job.
type OpenSubtitleLibrary struct {
	Request, Fingerprint string
	Query                opensubtitles.Query
}
type OpenSubtitleOperation struct{ Kind, Signature, State, Digest string }
type OpenSubtitleReceipt struct {
	ID                              string                  `json:"id"`
	Reference                       string                  `json:"reference"`
	Name                            string                  `json:"name"`
	State                           string                  `json:"state"`
	Provider                        string                  `json:"provider"`
	Origin                          string                  `json:"origin"`
	Created                         string                  `json:"created"`
	Updated                         string                  `json:"updated"`
	Reason                          string                  `json:"reason"`
	LibraryID                       string                  `json:"libraryId"`
	Requests                        int                     `json:"requests"`
	Outputs                         []LibraryOutput         `json:"outputs"`
	Definition                      OpenSubtitleDefinition  `json:"-"`
	Fingerprint, Binding, NextCheck string                  `json:"-"`
	Operations                      []OpenSubtitleOperation `json:"-"`
}

func OpenSubtitleID(kind, reference string, ordinal int) string {
	return Digest([]string{"open-subtitle-v1", kind, reference, strconv.Itoa(ordinal)})
}
func openSubtitleProducer(d OpenSubtitleDefinition) string {
	return Digest([]any{d.Kind, d.Reference, d.Ordinal, d.Origin, d.File, d.Extension, d.Bluray, openSubtitleMedia(d.Media), d.Source})
}
func openSubtitleMedia(v map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"title", "en_name", "cn_name", "year", "type", "tmdb_id", "season_episode", "imdb_id", "season", "episode"} {
		if value, ok := v[key]; ok {
			out[key] = value
		}
	}
	return out
}
func openSubtitleValid(d OpenSubtitleDefinition) bool {
	if d.Version != 1 || !d.Provider.Valid() || !recoveryHex(d.Origin, 64) || d.Ordinal < 0 || d.Ordinal > 50000 || !path.IsAbs(d.File) || path.Clean(d.File) != d.File || !subtitleRequestText(d.File, 4096) || d.Media == nil || len(d.Media) > 20 {
		return false
	}
	if _, e := time.Parse(time.RFC3339Nano, d.Created); e != nil {
		return false
	}
	if d.Kind == "intake" {
		if !recoveryHex(d.Reference, 32) || d.Source == nil || !d.Source.Valid() || d.Source.Reference != d.Reference || d.Source.Ordinal != d.Ordinal || d.Source.Origin != d.Origin {
			return false
		}
	} else if d.Kind == "manual" {
		if !recoveryHex(d.Reference, 64) || d.Ordinal != 0 || d.Source != nil || d.Bluray {
			return false
		}
	} else {
		return false
	}
	if d.Extension != "" && (path.Base(d.Extension) != d.Extension || !strings.HasPrefix(d.Extension, ".") || len(d.Extension) > 20 || FileKind("video"+d.Extension) != "media") {
		return false
	}
	if !d.Bluray && (d.Extension == "" || FileKind(d.File) != "media") {
		return false
	}
	title, ok := d.Media["title"].(string)
	if !ok || !subtitleRequestText(title, 600) {
		return false
	}
	kind, ok := d.Media["type"].(string)
	if !ok || kind != "电影" && kind != "电视剧" && kind != "动漫" {
		return false
	}
	tmdb, ok := d.Media["tmdb_id"].(string)
	if !ok {
		return false
	}
	id, e := strconv.ParseInt(tmdb, 10, 64)
	return e == nil && id > 0 && strconv.FormatInt(id, 10) == tmdb
}
func emitOpenSubtitle(ctx context.Context, tx *sql.Tx, d OpenSubtitleDefinition, expected *opensubtitles.Snapshot) error {
	if e := openSubtitleSchema(ctx, tx); e != nil {
		return e
	}
	id := OpenSubtitleID(d.Kind, d.Reference, d.Ordinal)
	producer := openSubtitleProducer(d)
	var observed string
	var captured int
	old := tx.QueryRowContext(ctx, `SELECT PRODUCER,CAPTURED FROM GO_OPEN_SUBTITLE_OBSERVATIONS WHERE ID=?`, id).Scan(&observed, &captured)
	if old == nil {
		if observed != producer || captured != 0 && captured != 1 {
			return ErrState
		}
		if captured == 1 {
			saved, e := openSubtitleRead(ctx, tx, id)
			if e != nil || openSubtitleProducer(saved.Definition) != producer || expected != nil && Digest(saved.Definition.Provider) != Digest(*expected) {
				return ErrState
			}
		} else {
			var exists bool
			if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_OPEN_SUBTITLE_REQUESTS WHERE ID=?)`, id).Scan(&exists); e != nil || exists || expected != nil {
				return ErrState
			}
		}
		return nil
	}
	if !errors.Is(old, sql.ErrNoRows) {
		return old
	}
	_, scope, wanted, ready, e := opensubtitles.Current(ctx, tx)
	capture := 0
	if e == nil && wanted && ready {
		capture = 1
	}
	if expected != nil && (capture == 0 || Digest(scope) != Digest(*expected)) {
		return ErrState
	}
	res, e := tx.ExecContext(ctx, `INSERT INTO GO_OPEN_SUBTITLE_OBSERVATIONS(ID,PRODUCER,CAPTURED)VALUES(?,?,?)`, id, producer, capture)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var check string
	var flag int
	if e = tx.QueryRowContext(ctx, `SELECT PRODUCER,CAPTURED FROM GO_OPEN_SUBTITLE_OBSERVATIONS WHERE ID=?`, id).Scan(&check, &flag); e != nil || check != producer || flag != capture {
		return ErrState
	}
	if capture == 0 {
		return nil
	}
	d.Version = 1
	d.Provider = scope
	d.Created = time.Now().UTC().Format(time.RFC3339Nano)
	d.Media = openSubtitleMedia(d.Media)
	if d.Kind == "intake" {
		d.RemoteWrite = d.RemoteWrite && scope.Remote
	}
	if !openSubtitleValid(d) {
		return ErrState
	}
	raw, _ := json.Marshal(d)
	fp := Digest(d)
	if len(raw) > opensubtitles.MaxDocument {
		return ErrLimit
	}
	res, e = tx.ExecContext(ctx, `INSERT INTO GO_OPEN_SUBTITLE_REQUESTS(ID,KIND,REFERENCE,ORDINAL,DOCUMENT,FINGERPRINT,STATE,CREATED,UPDATED)VALUES(?,?,?,?,?,?,'pending',?,?)`, id, d.Kind, d.Reference, d.Ordinal, string(raw), fp, d.Created, d.Created)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	saved, e := openSubtitleRead(ctx, tx, id)
	if e != nil || saved.Fingerprint != fp || saved.State != "pending" || saved.Binding != "" || saved.LibraryID != "" || saved.Requests != 0 {
		return ErrState
	}
	return nil
}

// Source readiness is checked using the actual intent transaction. Manual
// parent decoding deliberately does not call its child projection recursively.
func openSubtitleSourceReady(ctx context.Context, tx *sql.Tx, d OpenSubtitleDefinition) error {
	if !openSubtitleValid(d) {
		return ErrState
	}
	if d.Kind == "intake" {
		if e := SubtitleIntakeReady(ctx, tx, d.Reference, d.Ordinal, d.Origin, d.Source, true); e != nil {
			return e
		}
		var raw string
		if e := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, d.Reference).Scan(&raw); e != nil {
			return e
		}
		var original Definition
		if opensubtitles.DecodeLimit(raw, &original, 32<<20) != nil || d.Ordinal >= len(original.Entries) {
			return ErrState
		}
		entry := original.Entries[d.Ordinal]
		if original.Remote != nil && (!d.RemoteWrite || !d.Provider.Remote) || original.Remote == nil && d.RemoteWrite {
			return ErrMode
		}
		if entry.Kind != "media" || path.Join(original.TargetRoot, entry.Target) != d.File || IsDisc(entry) != d.Bluray || d.Media["title"] != entry.Title || d.Media["tmdb_id"] != entry.TMDBID || d.Media["type"] != entry.MediaType || d.Media["year"] != entry.Year || d.Media["season_episode"] != entry.SeasonEpisode {
			return ErrState
		}
		return nil
	}
	var raw, fp, state string
	if e := tx.QueryRowContext(ctx, `SELECT DOCUMENT,FINGERPRINT,STATE FROM GO_MANUAL_SUBTITLE_REQUESTS WHERE ID=?`, d.Reference).Scan(&raw, &fp, &state); e != nil {
		return e
	}
	var parent SubtitleRequestDefinition
	if opensubtitles.Decode(raw, &parent) != nil || !subtitleRequestValid(parent) || Digest(parent) != fp || state != "queued" || fp != d.Origin || parent.OpenSubtitles == nil || Digest(*parent.OpenSubtitles) != Digest(d.Provider) || path.Join(parent.Scope.Root.Path, parent.Selection) != d.File || Digest(openSubtitleMedia(parent.MediaInfo)) != Digest(d.Media) || d.RemoteWrite != (parent.Scope.Remote != nil) {
		return ErrState
	}
	return nil
}
func openSubtitleCurrent(ctx context.Context, tx *sql.Tx, d OpenSubtitleDefinition) error {
	_, scope, wanted, ready, e := opensubtitles.Current(ctx, tx)
	if e != nil {
		return e
	}
	if !wanted || !ready || Digest(scope) != Digest(d.Provider) {
		return ErrState
	}
	return openSubtitleSourceReady(ctx, tx, d)
}
func (s *Store) OpenSubtitleReady(ctx context.Context, p OpenSubtitleReceipt) error {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return ErrState
	}
	return openSubtitleCurrent(ctx, tx, p.Definition)
}
func (s *Store) OpenSubtitleProvider(ctx context.Context) (opensubtitles.Config, opensubtitles.Snapshot, bool, bool, error) {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return opensubtitles.Config{}, opensubtitles.Snapshot{}, false, false, e
	}
	defer tx.Rollback()
	return opensubtitles.Current(ctx, tx)
}
func OpenSubtitleName(d OpenSubtitleDefinition, extension string) string {
	if opensubtitles.Extension("subtitle"+extension) != extension {
		return ""
	}
	stem := path.Base(d.File)
	if !d.Bluray {
		stem = strings.TrimSuffix(stem, path.Ext(stem))
	}
	// Keep the original helper's splitext naming, including an empty source
	// suffix for a proven disc. No archive member supplies a destination name.
	intermediate := stem + ".chi.zh-cn" + d.Extension
	return strings.TrimSuffix(intermediate, path.Ext(intermediate)) + extension
}
func openSubtitleSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_OPEN_SUBTITLE_OBSERVATIONS(ID TEXT PRIMARY KEY,PRODUCER TEXT NOT NULL,CAPTURED INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_OPEN_SUBTITLE_REQUESTS(ID TEXT PRIMARY KEY,KIND TEXT NOT NULL,REFERENCE TEXT NOT NULL,ORDINAL INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',LIBRARY_ID TEXT NOT NULL DEFAULT '',BINDING TEXT NOT NULL DEFAULT '',OPERATIONS TEXT NOT NULL DEFAULT '[]',NEXT_CHECK TEXT NOT NULL DEFAULT '',UNIQUE(KIND,REFERENCE,ORDINAL))`,
		`CREATE INDEX IF NOT EXISTS GO_OPEN_SUBTITLE_PENDING ON GO_OPEN_SUBTITLE_REQUESTS(STATE,NEXT_CHECK,CREATED,ID)`,
		`CREATE TABLE IF NOT EXISTS GO_OPEN_SUBTITLE_AUTH_BLOCKS(SCOPE TEXT PRIMARY KEY,REQUEST_ID TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, e := tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	return nil
}

const openSubtitleSelect = `SELECT ID,KIND,REFERENCE,ORDINAL,DOCUMENT,FINGERPRINT,STATE,CREATED,UPDATED,REASON,LIBRARY_ID,BINDING,OPERATIONS,NEXT_CHECK FROM GO_OPEN_SUBTITLE_REQUESTS`

func openSubtitleRead(ctx context.Context, r recoveryReader, id string) (OpenSubtitleReceipt, error) {
	p := OpenSubtitleReceipt{Outputs: []LibraryOutput{}, Operations: []OpenSubtitleOperation{}}
	exists, e := recoveryTable(ctx, r, "GO_OPEN_SUBTITLE_REQUESTS")
	if e != nil {
		return p, e
	}
	if !exists {
		return p, ErrNotFound
	}
	var kind, reference, raw, operations string
	var ordinal int
	e = r.QueryRowContext(ctx, openSubtitleSelect+` WHERE ID=?`, id).Scan(&p.ID, &kind, &reference, &ordinal, &raw, &p.Fingerprint, &p.State, &p.Created, &p.Updated, &p.Reason, &p.LibraryID, &p.Binding, &operations, &p.NextCheck)
	if errors.Is(e, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if e != nil {
		return p, e
	}
	d := OpenSubtitleDefinition{}
	if opensubtitles.Decode(raw, &d) != nil || !openSubtitleValid(d) || Digest(d) != p.Fingerprint || d.Kind != kind || d.Reference != reference || d.Ordinal != ordinal || d.Created != p.Created || id != p.ID || id != OpenSubtitleID(kind, reference, ordinal) || opensubtitles.Decode(operations, &p.Operations) != nil || len(p.Operations) > 256 {
		return p, ErrState
	}
	var producer string
	var captured int
	if e = r.QueryRowContext(ctx, `SELECT PRODUCER,CAPTURED FROM GO_OPEN_SUBTITLE_OBSERVATIONS WHERE ID=?`, id).Scan(&producer, &captured); e != nil || captured != 1 || producer != openSubtitleProducer(d) {
		return p, ErrState
	}
	if _, e = time.Parse(time.RFC3339Nano, p.Updated); e != nil {
		return p, ErrState
	}
	if p.NextCheck != "" {
		if _, e = time.Parse(time.RFC3339Nano, p.NextCheck); e != nil {
			return p, ErrState
		}
	}
	if len(p.Reason) > 1000 || strings.ContainsAny(p.Reason, "\x00\r\n") {
		return p, ErrState
	}
	switch p.State {
	case "pending", "running", "completed", "not_found", "partial", "blocked", "uncertain", "stopped":
	default:
		return p, ErrState
	}
	for i, op := range p.Operations {
		if op.Kind != "login" && op.Kind != "search" && op.Kind != "archive" && op.Kind != "download-grant" && op.Kind != "subtitle" {
			return p, ErrState
		}
		if !recoveryHex(op.Signature, 64) || op.State != "intent" && op.State != "received" && op.State != "denied" && op.State != "uncertain" || op.State == "intent" && i != len(p.Operations)-1 || op.Digest != "" && !recoveryHex(op.Digest, 64) {
			return p, ErrState
		}
	}
	p.Definition = d
	p.Provider = d.Provider.Provider
	p.Origin = d.Provider.Origin
	p.Reference = d.Reference
	p.Name = d.Media["title"].(string)
	p.Requests = len(p.Operations)
	if p.LibraryID == "" {
		if p.Binding != "" || p.Requests > 0 || p.State != "pending" && p.State != "blocked" && p.State != "stopped" {
			return p, ErrState
		}
	} else {
		if !recoveryHex(p.LibraryID, 64) || !recoveryHex(p.Binding, 64) {
			return p, ErrState
		}
		var library, media string
		if e = r.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, p.LibraryID).Scan(&library); e != nil {
			return p, ErrState
		}
		if e = r.QueryRowContext(ctx, `SELECT MEDIA FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=0`, p.LibraryID).Scan(&media); e != nil {
			return p, ErrState
		}
		var ld LibraryDefinition
		var lm LibraryMedia
		if opensubtitles.DecodeLimit(library, &ld, 1<<20) != nil || opensubtitles.DecodeLimit(media, &lm, LibraryWorklistBytes) != nil || ld.OpenSubtitle == nil || ld.OpenSubtitle.Request != p.ID || ld.OpenSubtitle.Fingerprint != p.Fingerprint || !ld.OpenSubtitle.Query.Valid() || ld.Actor != d.Provider.Actor || ld.Mode != "no_force" || ld.Count != 1 || path.Join(ld.Root.Path, lm.Path) != d.File || Digest([]any{ld, lm}) != p.Binding {
			return p, ErrState
		}
	}
	return p, nil
}
func (s *Store) OpenSubtitle(ctx context.Context, id string) (OpenSubtitleReceipt, error) {
	if !recoveryHex(id, 64) {
		return OpenSubtitleReceipt{}, ErrPath
	}
	p, e := openSubtitleRead(ctx, s.db, id)
	if e != nil {
		return p, e
	}
	if p.LibraryID != "" {
		p.Outputs, e = s.LibraryOutputs(ctx, p.LibraryID, 1)
	}
	return p, e
}
func (s *Store) OpenSubtitles(ctx context.Context, pending bool, page int) ([]OpenSubtitleReceipt, error) {
	out := []OpenSubtitleReceipt{}
	if page < 1 || page > 10000 {
		return out, ErrPath
	}
	yes, e := recoveryTable(ctx, s.db, "GO_OPEN_SUBTITLE_REQUESTS")
	if e != nil || !yes {
		return out, e
	}
	q := `SELECT ID FROM GO_OPEN_SUBTITLE_REQUESTS ORDER BY CREATED DESC,ID LIMIT 30 OFFSET ?`
	args := []any{(page - 1) * 30}
	if pending {
		q = `SELECT ID FROM GO_OPEN_SUBTITLE_REQUESTS WHERE STATE='pending' AND (NEXT_CHECK='' OR NEXT_CHECK<=?) ORDER BY CREATED,ID LIMIT 20`
		args = []any{time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")}
	}
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return out, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return out, e
	}
	for _, id := range ids {
		p, e := s.OpenSubtitle(ctx, id)
		if e != nil {
			return out, e
		}
		out = append(out, p)
	}
	return out, nil
}
