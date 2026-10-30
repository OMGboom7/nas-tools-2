package organization

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

const manualSubtitlePrefix = "manual-subtitle:"

// A manual request adopts a reviewed existing video. It never fabricates an
// intake job, transfer history, successful download, or a completed subtitle.
type SubtitleRequestDefinition struct {
	OpenSubtitles                                                             *opensubtitles.Snapshot `json:",omitempty"`
	Version                                                                   int
	Actor, Authority, RequestID, Created                                      string
	Scope                                                                     RecoveryScope
	Allowed                                                                   []LibraryRoot
	Selection, InputPath, Name, Title, Year, MediaType, TMDBID, ContentDigest string
	MediaInfo                                                                 map[string]any
	Local                                                                     *LibraryMedia
	RemoteNodes                                                               []RecoveryNode
	Plugin                                                                    subtitles.Snapshot
}
type SubtitleRequest struct {
	OpenSubtitles  *OpenSubtitleReceipt      `json:"openSubtitles,omitempty"`
	ID             string                    `json:"id"`
	State          string                    `json:"state"`
	Queued         bool                      `json:"queued"`
	RootID         string                    `json:"rootId"`
	Driver         string                    `json:"driver"`
	Path           string                    `json:"path"`
	Name           string                    `json:"name"`
	Title          string                    `json:"title"`
	Year           string                    `json:"year"`
	MediaType      string                    `json:"mediaType"`
	TMDBID         string                    `json:"tmdbId"`
	Service        string                    `json:"service"`
	ServiceOrigin  string                    `json:"serviceOrigin"`
	MappedPath     string                    `json:"mappedPath"`
	HTTP           bool                      `json:"http"`
	Bytes          int64                     `json:"bytes"`
	Created        string                    `json:"created"`
	Updated        string                    `json:"updated"`
	Fingerprint    string                    `json:"fingerprint"`
	Reason         string                    `json:"reason"`
	ServiceState   string                    `json:"serviceState"`
	ServiceUpdated string                    `json:"serviceUpdated"`
	ExternalID     string                    `json:"externalId"`
	StoppedBy      string                    `json:"stoppedBy"`
	Definition     SubtitleRequestDefinition `json:"-"`
}

func SubtitleRequestID(actor, request string) string {
	return Digest([]string{"manual-subtitle", actor, request})
}
func SubtitleRequestSource(id string) string { return manualSubtitlePrefix + id }
func SubtitleRequestSourceID(source string) (string, bool) {
	id, ok := strings.CutPrefix(source, manualSubtitlePrefix)
	return id, ok && recoveryHex(id, 64)
}
func subtitleRequestText(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func SubtitleRequestPath(selected string) bool {
	if !validRelative(selected) || selected == "." || FileKind(selected) != "media" {
		return false
	}
	for _, part := range strings.Split(selected, "/") {
		if strings.HasPrefix(strings.ToLower(part), ".nastool-") {
			return false
		}
	}
	return true
}
func subtitleRequestLibrary(d SubtitleRequestDefinition) LibraryDefinition {
	return LibraryDefinition{Root: d.Scope.Root, Allowed: d.Allowed, Selection: d.Selection, Mode: "no_force", Actor: d.Actor, RequestID: d.RequestID, ConfigDigest: d.Scope.ConfigDigest}
}
func subtitleRequestValid(d SubtitleRequestDefinition) bool {
	if d.Version != 1 && d.Version != 2 || d.Version == 1 && d.OpenSubtitles != nil || d.Version == 2 && (d.OpenSubtitles == nil || !d.OpenSubtitles.Valid() || d.OpenSubtitles.Actor != d.Actor || d.OpenSubtitles.Authority != d.Authority) || !subtitleRequestText(d.Actor, 256) || !recoveryHex(d.Authority, 64) || !recoveryHex(d.RequestID, 32) || !recoveryScopeValid(d.Scope) || !SubtitleRequestPath(d.Selection) || !subtitleRequestText(d.Name, 600) || !subtitleRequestText(d.Title, 600) || !recoveryHex(d.ContentDigest, 64) || len(d.Allowed) > 256 || len(d.MediaInfo) > 1000 {
		return false
	}
	if _, e := time.Parse(time.RFC3339Nano, d.Created); e != nil {
		return false
	}
	if d.InputPath != "" && (!utf8.ValidString(d.InputPath) || len(d.InputPath) > 4096 || strings.ContainsAny(d.InputPath, "\x00\r\n")) {
		return false
	}
	if d.MediaType != "电影" && d.MediaType != "电视剧" && d.MediaType != "动漫" {
		return false
	}
	id, e := strconv.ParseInt(d.TMDBID, 10, 64)
	if e != nil || id <= 0 || strconv.FormatInt(id, 10) != d.TMDBID {
		return false
	}
	if len(d.Year) > 4 || strings.Trim(d.Year, "0123456789") != "" || d.MediaInfo == nil || d.MediaInfo["title"] != d.Title || d.MediaInfo["year"] != d.Year || d.MediaInfo["type"] != d.MediaType || d.MediaInfo["tmdb_id"] != d.TMDBID {
		return false
	}
	videoType := 1
	if d.MediaType == "电影" {
		videoType = 0
	}
	if d.Plugin.Basis == "" {
		if d.Version != 2 || Digest(d.Plugin) != Digest(subtitles.Snapshot{}) {
			return false
		}
	} else if !recoveryHex(d.Plugin.Basis, 64) || !recoveryHex(d.Plugin.ConfigDigest, 64) || d.Plugin.Payload.Bluray || d.Plugin.Payload.Path == "" || d.Plugin.Payload.Priority != 3 || d.Plugin.Payload.MediaID != "" || d.Plugin.Payload.VideoType != videoType {
		return false
	}
	if d.Scope.Remote == nil {
		if d.Local == nil || d.Local.Disc != nil || d.Local.Remote != nil || d.Local.Path != d.Selection || d.Local.Blocked != "" || len(d.RemoteNodes) != 0 {
			return false
		}
	} else {
		if d.Local != nil || len(d.Allowed) != 0 || len(d.RemoteNodes) != 1 || d.RemoteNodes[0].Kind != "file" || d.RemoteNodes[0].Path != "." || d.RemoteNodes[0].Remote == nil || d.RemoteNodes[0].Digest != d.ContentDigest {
			return false
		}
	}
	return true
}
func SubtitleRequestLibrary(d SubtitleRequestDefinition) LibraryDefinition {
	return subtitleRequestLibrary(d)
}

// Content verification is readonly and cancellable. Known configured roots
// bound local links; remote reads use only the frozen provider binding.
func VerifySubtitleRequestSource(ctx context.Context, d SubtitleRequestDefinition, c *remotestore.Client) error {
	if !subtitleRequestValid(d) {
		return ErrState
	}
	if d.Scope.Remote != nil {
		nodes, e := MediaRemovalSnapshot(ctx, d.Scope, d.Selection, c)
		if e != nil {
			return e
		}
		if Digest(nodes) != Digest(d.RemoteNodes) {
			return ErrState
		}
		return nil
	}
	digest, e := SubtitleRequestLocalDigest(ctx, subtitleRequestLibrary(d), *d.Local)
	if e != nil {
		return e
	}
	if digest != d.ContentDigest {
		return ErrState
	}
	return nil
}
func SubtitleRequestLocalDigest(ctx context.Context, d LibraryDefinition, m LibraryMedia) (string, error) {
	if m.Disc != nil || m.Remote != nil {
		return "", ErrMode
	}
	raw, e := ReadLibraryMedia(ctx, d, m, func(f *os.File) ([]byte, error) {
		h := sha256.New()
		buf := make([]byte, 256<<10)
		var count int64
		for {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			n, e := f.Read(buf)
			if n > 0 {
				count += int64(n)
				if count > RecoveryByteLimit {
					return nil, ErrLimit
				}
				_, _ = h.Write(buf[:n])
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			if n == 0 {
				return nil, io.ErrNoProgress
			}
		}
		if count != m.Input.Size {
			return nil, ErrState
		}
		return h.Sum(nil), nil
	})
	if e != nil {
		return "", e
	}
	return hex.EncodeToString(raw), nil
}
func subtitleRequestSchema(ctx context.Context, tx *sql.Tx) error {
	_, e := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_MANUAL_SUBTITLE_REQUESTS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,QUEUED INTEGER NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,REASON TEXT NOT NULL,STOPPED_BY TEXT NOT NULL DEFAULT '')`)
	return e
}
func subtitleRequestRead(ctx context.Context, r recoveryReader, id string) (SubtitleRequest, error) {
	p := SubtitleRequest{}
	yes, e := recoveryTable(ctx, r, "GO_MANUAL_SUBTITLE_REQUESTS")
	if e != nil {
		return p, e
	}
	if !yes {
		return p, ErrNotFound
	}
	var raw, fp, created string
	var queued int
	e = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,FINGERPRINT,STATE,QUEUED,CREATED,UPDATED,REASON,STOPPED_BY FROM GO_MANUAL_SUBTITLE_REQUESTS WHERE ID=?`, id).Scan(&p.ID, &raw, &fp, &p.State, &queued, &created, &p.Updated, &p.Reason, &p.StoppedBy)
	if errors.Is(e, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if e != nil {
		return p, e
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	d := SubtitleRequestDefinition{}
	if len(raw) > 512<<10 || !json.Valid([]byte(raw)) || dec.Decode(&d) != nil || !subtitleRequestValid(d) || Digest(d) != fp || id != SubtitleRequestID(d.Actor, d.RequestID) || p.ID != id || d.Created != created || queued != 0 && queued != 1 {
		return p, ErrState
	}
	if _, e := time.Parse(time.RFC3339Nano, p.Updated); e != nil {
		return p, ErrState
	}
	if len(p.Reason) > 1000 || !utf8.ValidString(p.Reason) || strings.ContainsAny(p.Reason, "\x00\r\n") {
		return p, ErrState
	}
	if p.State != "review" && p.State != "queued" && p.State != "cancelled" || p.State == "review" && queued != 0 || p.State == "queued" && queued != 1 {
		return p, ErrState
	}
	if p.State == "cancelled" && (!subtitleRequestText(p.StoppedBy, 256) || strings.TrimSpace(p.Reason) == "") || p.State != "cancelled" && p.StoppedBy != "" {
		return p, ErrState
	}
	p.Queued = queued == 1
	p.Definition = d
	p.Fingerprint = fp
	p.Created = d.Created
	p.RootID = d.Scope.Root.ID
	p.Path = d.Selection
	p.Name = d.Name
	p.Title = d.Title
	p.Year = d.Year
	p.MediaType = d.MediaType
	p.TMDBID = d.TMDBID
	p.Service = subtitles.Plugin
	p.ServiceOrigin = d.Plugin.Origin
	p.MappedPath = d.Plugin.Payload.Path
	p.HTTP = d.Plugin.HTTP
	if d.OpenSubtitles != nil {
		open := OpenSubtitleReceipt{ID: OpenSubtitleID("manual", id, 0), Reference: id, Name: d.Title, Provider: d.OpenSubtitles.Provider, Origin: d.OpenSubtitles.Origin, State: "not_queued", Created: d.Created, Outputs: []LibraryOutput{}}
		if p.Queued {
			saved, e := openSubtitleRead(ctx, r, open.ID)
			if e != nil || saved.Definition.Kind != "manual" || saved.Reference != id || saved.Definition.Origin != fp || Digest(saved.Definition.Provider) != Digest(*d.OpenSubtitles) || saved.Definition.File != path.Join(d.Scope.Root.Path, d.Selection) {
				return p, ErrState
			}
			open = saved
			if open.LibraryID != "" {
				rows, e := r.QueryContext(ctx, libraryOutputSelect+` WHERE JOB_ID=? ORDER BY TARGET LIMIT 4`, open.LibraryID)
				if e != nil {
					return p, e
				}
				for rows.Next() {
					out, e := scanLibraryOutput(rows)
					if e != nil {
						rows.Close()
						return p, e
					}
					open.Outputs = append(open.Outputs, out)
				}
				e = rows.Err()
				rows.Close()
				if e != nil || len(open.Outputs) > 3 {
					return p, ErrState
				}
			}
		}
		p.OpenSubtitles = &open
		if d.Plugin.Basis == "" {
			p.Service = opensubtitles.Plugin
			p.ServiceOrigin = d.OpenSubtitles.Origin
			p.MappedPath = strings.TrimSuffix(path.Join(d.Scope.Root.Path, d.Selection), path.Ext(d.Selection)) + ".chi.zh-cn"
		} else {
			p.Service = subtitles.Plugin + "+" + opensubtitles.Plugin
		}
	}
	if d.Scope.Remote != nil {
		p.Driver = d.Scope.Remote.Driver
		p.Bytes = d.RemoteNodes[0].Node.Size
	} else {
		p.Bytes = d.Local.Input.Size
	}
	p.ServiceState = "not_queued"
	if p.Queued && d.Plugin.Basis != "" {
		q, e := subtitles.Read(ctx, r, Digest([]string{SubtitleRequestSource(id), "0"}))
		if e != nil || q.JobID != SubtitleRequestSource(id) || q.Ordinal != 0 || q.Name != d.Name || q.LocalPath != path.Join(d.Scope.Root.Path, d.Selection) || q.MediaType != d.MediaType || q.OriginDigest != fp || q.ConfigDigest != d.Plugin.ConfigDigest || Digest(q.Payload) != Digest(d.Plugin.Payload) {
			return p, ErrState
		}
		p.ServiceState = q.State
		p.ServiceUpdated = q.Updated
		p.ExternalID = q.ExternalID
		if p.State == "cancelled" && q.State != "blocked" {
			return p, ErrState
		}
	}
	if p.Queued && d.Plugin.Basis == "" && p.OpenSubtitles != nil {
		p.ServiceUpdated = p.OpenSubtitles.Updated
		switch p.OpenSubtitles.State {
		case "running":
			p.ServiceState = "sending"
		case "stopped":
			p.ServiceState = "blocked"
		default:
			p.ServiceState = p.OpenSubtitles.State
		}
		if p.State == "cancelled" && p.ServiceState != "blocked" {
			return p, ErrState
		}
	}
	return p, nil
}
func (s *Store) SubtitleRequest(ctx context.Context, id string) (SubtitleRequest, error) {
	if !recoveryHex(id, 64) {
		return SubtitleRequest{}, ErrPath
	}
	return subtitleRequestRead(ctx, s.db, id)
}
func (s *Store) SubtitleRequests(ctx context.Context, page int) ([]SubtitleRequest, error) {
	out := []SubtitleRequest{}
	if page < 1 || page > 10000 {
		return nil, ErrPath
	}
	yes, e := recoveryTable(ctx, s.db, "GO_MANUAL_SUBTITLE_REQUESTS")
	if e != nil || !yes {
		return out, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT ID FROM GO_MANUAL_SUBTITLE_REQUESTS ORDER BY CREATED DESC,ID LIMIT 30 OFFSET ?`, (page-1)*30)
	if e != nil {
		return nil, e
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
		return nil, e
	}
	for _, id := range ids {
		p, e := s.SubtitleRequest(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
func subtitleRequestCurrent(ctx context.Context, tx *sql.Tx, d SubtitleRequestDefinition, validate func() error) error {
	if validate == nil || validate() != nil {
		return ErrState
	}
	snap, wanted, e := subtitles.OptionalSnapshotFor(ctx, tx, path.Join(d.Scope.Root.Path, d.Selection), d.MediaType, false)
	if e != nil {
		return e
	}
	if wanted != (d.Plugin.Basis != "") || Digest(snap) != Digest(d.Plugin) {
		return ErrState
	}
	_, open, wanted, ready, e := opensubtitles.Current(ctx, tx)
	if e != nil {
		return e
	}
	if d.OpenSubtitles == nil {
		if wanted {
			return ErrState
		}
	} else if !wanted || !ready || Digest(open) != Digest(*d.OpenSubtitles) {
		return ErrState
	}
	return nil
}
func (s *Store) SaveSubtitleReview(ctx context.Context, d SubtitleRequestDefinition, validate func() error) (SubtitleRequest, error) {
	if !subtitleRequestValid(d) {
		return SubtitleRequest{}, ErrPath
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return SubtitleRequest{}, e
	}
	defer tx.Rollback()
	if e = subtitleRequestCurrent(ctx, tx, d, validate); e != nil {
		return SubtitleRequest{}, e
	}
	if e = subtitleRequestSchema(ctx, tx); e != nil {
		return SubtitleRequest{}, e
	}
	raw, _ := json.Marshal(d)
	if len(raw) > 512<<10 {
		return SubtitleRequest{}, ErrLimit
	}
	id := SubtitleRequestID(d.Actor, d.RequestID)
	res, e := tx.ExecContext(ctx, `INSERT INTO GO_MANUAL_SUBTITLE_REQUESTS(ID,DOCUMENT,FINGERPRINT,STATE,QUEUED,CREATED,UPDATED,REASON)VALUES(?,?,?,'review',0,?,?,'')`, id, string(raw), Digest(d), d.Created, d.Created)
	if e != nil {
		return SubtitleRequest{}, e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return SubtitleRequest{}, ErrState
	}
	p, e := subtitleRequestRead(ctx, tx, id)
	if e != nil || p.State != "review" || p.Fingerprint != Digest(d) || p.Updated != d.Created {
		return p, ErrState
	}
	if e = subtitleRequestCurrent(ctx, tx, d, validate); e != nil {
		return p, e
	}
	if e = tx.Commit(); e != nil {
		return p, e
	}
	return p, nil
}
func (s *Store) QueueSubtitleRequest(ctx context.Context, id, actor, fingerprint string, validate func() error, openRemote ...bool) (SubtitleRequest, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return SubtitleRequest{}, e
	}
	defer tx.Rollback()
	p, e := subtitleRequestRead(ctx, tx, id)
	if e != nil {
		return p, e
	}
	if p.Definition.Actor != actor || p.Fingerprint != fingerprint {
		return p, ErrState
	}
	if p.State == "queued" {
		return p, nil
	}
	created, _ := time.Parse(time.RFC3339Nano, p.Created)
	if p.State != "review" || time.Since(created) < 0 || time.Since(created) > 10*time.Minute {
		return p, ErrState
	}
	d := p.Definition
	if len(openRemote) > 1 || d.OpenSubtitles != nil && d.Scope.Remote != nil && (len(openRemote) != 1 || !openRemote[0]) {
		return p, ErrState
	}
	if e = subtitleRequestCurrent(ctx, tx, d, validate); e != nil {
		return p, e
	}
	source, file := SubtitleRequestSource(id), path.Join(d.Scope.Root.Path, d.Selection)
	if d.Plugin.Basis != "" {
		if e = subtitles.Emit(ctx, tx, source, 0, d.Name, file, d.MediaType, p.Fingerprint, false); e != nil {
			return p, e
		}
	}
	if d.OpenSubtitles != nil {
		if e = emitOpenSubtitle(ctx, tx, OpenSubtitleDefinition{Kind: "manual", Reference: id, Origin: p.Fingerprint, File: file, Extension: path.Ext(d.Selection), Media: d.MediaInfo, RemoteWrite: d.Scope.Remote != nil}, d.OpenSubtitles); e != nil {
			return p, e
		}
	}
	if e = emitSubtitleSignal(ctx, tx, "manual:"+id, id, file, path.Ext(d.Selection), false, d.MediaInfo, map[string]any{"stage": "queued", "request_id": id}); e != nil {
		return p, e
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, e := tx.ExecContext(ctx, `UPDATE GO_MANUAL_SUBTITLE_REQUESTS SET STATE='queued',QUEUED=1,UPDATED=? WHERE ID=? AND STATE='review' AND FINGERPRINT=?`, now, id, p.Fingerprint)
	if e != nil {
		return p, e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return p, ErrState
	}
	next, e := subtitleRequestRead(ctx, tx, id)
	if e != nil || next.State != "queued" || next.ServiceState != "pending" || next.Updated != now {
		return next, ErrState
	}
	if e = subtitleRequestCurrent(ctx, tx, d, validate); e != nil {
		return next, e
	}
	if e = tx.Commit(); e != nil {
		return next, e
	}
	return next, nil
}
func (s *Store) CancelSubtitleRequest(ctx context.Context, id, actor, reason string) (SubtitleRequest, error) {
	if !subtitleRequestText(reason, 1000) || strings.TrimSpace(reason) == "" || !subtitleRequestText(actor, 256) {
		return SubtitleRequest{}, ErrPath
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return SubtitleRequest{}, e
	}
	defer tx.Rollback()
	p, e := subtitleRequestRead(ctx, tx, id)
	if e != nil {
		return p, e
	}
	if p.State == "cancelled" {
		return p, ErrState
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if p.Queued {
		if p.ServiceState != "pending" || p.OpenSubtitles != nil && p.OpenSubtitles.State != "pending" && p.OpenSubtitles.State != "stopped" {
			return p, ErrState
		}
		if p.OpenSubtitles != nil && p.OpenSubtitles.State == "pending" {
			if _, e := stopOpenSubtitle(ctx, tx, p.OpenSubtitles.ID, actor, reason); e != nil {
				return p, e
			}
		}
		if p.Definition.Plugin.Basis != "" {
			qid := Digest([]string{SubtitleRequestSource(id), "0"})
			res, e := tx.ExecContext(ctx, `UPDATE GO_SUBTITLE_JOBS SET STATE='blocked',REASON='original manual request explicitly stopped; no replay',UPDATED=? WHERE ID=? AND STATE='pending'`, now, qid)
			if e != nil {
				return p, e
			}
			if n, e := res.RowsAffected(); e != nil || n != 1 {
				return p, ErrState
			}
			q, e := subtitles.Read(ctx, tx, qid)
			if e != nil || q.State != "blocked" || q.Updated != now || q.Reason != "original manual request explicitly stopped; no replay" {
				return p, ErrState
			}
		}
	}
	res, e := tx.ExecContext(ctx, `UPDATE GO_MANUAL_SUBTITLE_REQUESTS SET STATE='cancelled',UPDATED=?,REASON=?,STOPPED_BY=? WHERE ID=? AND STATE=? AND UPDATED=?`, now, reason, actor, id, p.State, p.Updated)
	if e != nil {
		return p, e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return p, ErrState
	}
	next, e := subtitleRequestRead(ctx, tx, id)
	if e != nil || next.State != "cancelled" || next.Updated != now || next.Reason != reason || next.StoppedBy != actor {
		return next, ErrState
	}
	if e = tx.Commit(); e != nil {
		return next, e
	}
	return next, nil
}
