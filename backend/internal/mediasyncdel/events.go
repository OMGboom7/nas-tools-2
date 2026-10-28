package mediasyncdel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"strings"
)

type Definition struct {
	Control  Control
	Spec     Spec
	Event    Event
	Callback string
}
type Receipt struct {
	ID                string     `json:"id"`
	Callback          string     `json:"callbackId"`
	Event             Event      `json:"event"`
	State             string     `json:"state"`
	Parent            string     `json:"parentId"`
	ParentState       string     `json:"parentState"`
	Count             int        `json:"count"`
	Reason            string     `json:"reason"`
	Actor             string     `json:"actor"`
	Created           string     `json:"created"`
	Updated           string     `json:"updated"`
	Definition        Definition `json:"-"`
	Fingerprint       string     `json:"-"`
	ParentFingerprint string     `json:"parentFingerprint"`
	Image             string     `json:"-"`
	Policy            Spec       `json:"policy"`
}

func Capture(ctx context.Context, tx *sql.Tx, server, callback string, raw json.RawMessage) error {
	if server != "emby" {
		return nil
	}
	c, spec, active, err := Scope(ctx, tx)
	if errors.Is(err, ErrState) {
		return nil
	}
	if err != nil || !active {
		return err
	}
	e, handles, parseErr := Parse(raw)
	if !handles {
		return nil
	}
	state, reason := "pending", ""
	if parseErr != nil {
		e = Event{}
		state, reason = "blocked", "删除回调字段不完整或无效，没有处理历史或文件"
	}
	if e.Virtual {
		state, reason = "ignored", "虚拟媒体回调，不处理历史或文件"
	}
	if e.MissingVirtual {
		state, reason = "blocked", "缺少明确虚拟媒体标志，已停用此同步删除授权"
	}
	if !e.Virtual && !e.MissingVirtual && parseErr == nil && spec.Excluded(e.Path) {
		state, reason = "ignored", "原媒体路径位于已配置排除范围"
	}
	if !Hex(callback, 32) {
		return ErrState
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	id := Hash([]string{c.Epoch, callback})
	d := Definition{c, spec, e, callback}
	doc, _ := json.Marshal(d)
	at := now()
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_SYNC_DEL_EVENTS VALUES(?,?,?,?,'','','',0,'',?,'',?,?) ON CONFLICT(ID) DO NOTHING`, id, string(doc), Hash(d), state, reason, at, at)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	saved, err := ReadReceipt(ctx, tx, id)
	if err != nil || saved.Fingerprint != Hash(d) || saved.State != state || saved.Reason != reason {
		return ErrState
	}
	if e.MissingVirtual {
		original, err := Setting(ctx, tx, ConfigKey)
		if err != nil {
			return err
		}
		v, err := Values(original)
		if err != nil {
			return err
		}
		v["enable"] = false
		encoded, _ := json.Marshal(v)
		if err := setSetting(ctx, tx, ConfigKey, string(encoded)); err != nil {
			return err
		}
		c.Authorized = false
		c.ConfigDigest = ConfigDigest(string(encoded))
		return WriteControl(ctx, tx, c)
	}
	final, _, on, err := Scope(ctx, tx)
	if err != nil || !on || Hash(final) != Hash(c) {
		return ErrState
	}
	return nil
}
func ReadReceipt(ctx context.Context, r Reader, id string) (Receipt, error) {
	p := Receipt{}
	if !Hex(id, 64) {
		return p, ErrMissing
	}
	yes, err := exists(ctx, r, "GO_MEDIA_SYNC_DEL_EVENTS")
	if err != nil {
		return p, err
	}
	if !yes {
		return p, ErrMissing
	}
	var raw, link string
	err = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,FP,STATE,PARENT,PARENT_FP,LINK_FP,COUNT,IMAGE,REASON,ACTOR,CREATED,UPDATED FROM GO_MEDIA_SYNC_DEL_EVENTS WHERE ID=?`, id).Scan(&p.ID, &raw, &p.Fingerprint, &p.State, &p.Parent, &p.ParentFingerprint, &link, &p.Count, &p.Image, &p.Reason, &p.Actor, &p.Created, &p.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrMissing
	}
	if err != nil {
		return p, err
	}
	if len(raw) > 512<<10 || json.Unmarshal([]byte(raw), &p.Definition) != nil || Hash(p.Definition) != p.Fingerprint || p.ID != Hash([]string{p.Definition.Control.Epoch, p.Definition.Callback}) || !Hex(p.Definition.Callback, 32) {
		return p, ErrState
	}
	cRaw, _ := json.Marshal(p.Definition.Control)
	if _, err := DecodeControl(string(cRaw)); err != nil || !p.Definition.Control.Authorized {
		return p, ErrState
	}
	if p.Count < 0 || p.Count > HistoryLimit || p.Parent != "" && (!Hex(p.Parent, 64) || !Hex(p.ParentFingerprint, 64) || link != Hash([]any{p.Parent, p.ParentFingerprint, p.Count, p.Image})) {
		return p, ErrState
	}
	if p.Parent == "" && (p.Count != 0 || p.ParentFingerprint != "" || link != "" || p.Image != "" || p.State == "planned" || p.State == "running" || p.State == "needs_review" || p.State == "completed") {
		return p, ErrState
	}
	switch p.State {
	case "pending", "planned", "running", "needs_review", "completed", "ignored", "blocked":
	default:
		return p, ErrState
	}
	p.Event, p.Callback, p.Policy = p.Definition.Event, p.Definition.Callback, p.Definition.Spec
	if p.Parent != "" {
		err = r.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, p.Parent).Scan(&p.ParentState)
		if err != nil {
			return p, ErrState
		}
		if p.State != "completed" && p.State != "ignored" && (p.ParentState == "needs_review" || p.ParentState == "closed") {
			p.State = "needs_review"
		}
	}
	return p, nil
}
func (s *Store) Receipt(ctx context.Context, id string) (Receipt, error) {
	return ReadReceipt(ctx, s.db, id)
}
func (s *Store) List(ctx context.Context, page int, pending bool) ([]Receipt, error) {
	out := []Receipt{}
	if page < 1 || page > 10000 {
		return out, ErrLimit
	}
	yes, err := exists(ctx, s.db, "GO_MEDIA_SYNC_DEL_EVENTS")
	if err != nil || !yes {
		return out, err
	}
	query := `SELECT ID FROM GO_MEDIA_SYNC_DEL_EVENTS ORDER BY CREATED DESC,ID DESC LIMIT 30 OFFSET ?`
	args := []any{(page - 1) * 30}
	if pending {
		query = `SELECT ID FROM GO_MEDIA_SYNC_DEL_EVENTS WHERE STATE='pending' ORDER BY CREATED,ID LIMIT 10`
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		p, err := ReadReceipt(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func UpdateState(ctx context.Context, tx *sql.Tx, p Receipt, to, reason, actor string) error {
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_SYNC_DEL_EVENTS SET STATE=?,REASON=?,ACTOR=?,UPDATED=? WHERE ID=? AND FP=?`, to, reason, actor, now(), p.ID, p.Fingerprint)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var state, why, by string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON,ACTOR FROM GO_MEDIA_SYNC_DEL_EVENTS WHERE ID=?`, p.ID).Scan(&state, &why, &by); err != nil || state != to || why != reason || by != actor {
		return ErrState
	}
	return nil
}
func (s *Store) ResolvePending(ctx context.Context, p Receipt, state, reason string) error {
	if state != "blocked" && state != "ignored" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := ReadReceipt(ctx, tx, p.ID)
	if err != nil || current.Fingerprint != p.Fingerprint || current.State != "pending" || current.Parent != "" {
		return ErrState
	}
	if err := UpdateState(ctx, tx, current, state, reason, ""); err != nil {
		return err
	}
	return tx.Commit()
}
func CheckAutomatic(ctx context.Context, r Reader, receipt string, beforeComplete bool) error {
	p, err := ReadReceipt(ctx, r, receipt)
	if err != nil {
		return err
	}
	c, _, on, err := Scope(ctx, r)
	if err != nil || !on || Hash(c) != Hash(p.Definition.Control) || (p.State != "pending" && p.State != "planned" && p.State != "running" && p.State != "needs_review") {
		return ErrState
	}
	if beforeComplete && p.Count > 0 {
		ids, err := CandidateIDs(ctx, r, p.Event)
		if err != nil || len(ids) != p.Count {
			return ErrState
		}
		var raw string
		if err := r.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_MEDIA_REMOVALS WHERE ID=?`, p.Parent).Scan(&raw); err != nil {
			return err
		}
		var d struct {
			Histories []struct{ Record struct{ ID int64 } }
		}
		if json.Unmarshal([]byte(raw), &d) != nil {
			return ErrState
		}
		expected := []int64{}
		for _, h := range d.Histories {
			expected = append(expected, h.Record.ID)
		}
		if !SameIDs(ids, expected) {
			return ErrState
		}
	}
	return nil
}
func (s *Store) CheckAutomatic(ctx context.Context, id string) error {
	return CheckAutomatic(ctx, s.db, id, true)
}
func BindParent(ctx context.Context, tx *sql.Tx, receipt, parent, fp string, ids []int64, image string) error {
	p, err := ReadReceipt(ctx, tx, receipt)
	if err != nil || p.State != "pending" || p.Parent != "" || !Hex(parent, 64) || !Hex(fp, 64) || len(ids) == 0 || len(ids) > HistoryLimit {
		return ErrState
	}
	if err := CheckAutomatic(ctx, tx, receipt, false); err != nil {
		return err
	}
	selected, err := CandidateIDs(ctx, tx, p.Event)
	if err != nil || !SameIDs(selected, ids) {
		return ErrState
	}
	if image != "" && !SafeImage(image) {
		return ErrState
	}
	link := Hash([]any{parent, fp, len(ids), image})
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_SYNC_DEL_EVENTS SET PARENT=?,PARENT_FP=?,LINK_FP=?,COUNT=?,IMAGE=?,UPDATED=? WHERE ID=? AND STATE='pending'`, parent, fp, link, len(ids), image, now(), receipt)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	if err := UpdateState(ctx, tx, p, "planned", "完整原历史与源范围已冻结，尚未执行", ""); err != nil {
		return err
	}
	saved, err := ReadReceipt(ctx, tx, receipt)
	if err != nil || saved.Parent != parent || saved.ParentFingerprint != fp || saved.Count != len(ids) || saved.Image != image || saved.State != "planned" {
		return ErrState
	}
	return CheckAutomatic(ctx, tx, receipt, true)
}
func StartParent(ctx context.Context, tx *sql.Tx, receipt, parent, fp string) error {
	p, err := ReadReceipt(ctx, tx, receipt)
	if err != nil || p.Parent != parent || p.ParentFingerprint != fp || p.State == "ignored" || p.State == "completed" || p.State == "blocked" {
		return ErrState
	}
	return UpdateState(ctx, tx, p, "running", "已记录这次完整删除的执行意图，重启不自动续跑", "")
}
func CompleteParent(ctx context.Context, tx *sql.Tx, receipt, parent, fp string) error {
	p, err := ReadReceipt(ctx, tx, receipt)
	if err != nil || p.Parent != parent || p.ParentFingerprint != fp || p.ParentState != "completed" || p.State == "ignored" || p.State == "blocked" {
		return ErrState
	}
	if p.State == "completed" {
		return nil
	}
	if err := UpdateState(ctx, tx, p, "completed", "完整原范围已核实，原历史与去重记录同事务删除", ""); err != nil {
		return err
	}
	if p.Definition.Spec.Notify {
		e := p.Event
		label := "剧集"
		if e.Type == "Movie" {
			label = "电影"
		}
		season := ""
		if e.Type == "Season" {
			season = fmt.Sprintf(" S%02d", e.Season)
		} else if e.Type == "Episode" {
			season = " " + EpisodeKey(e)
		}
		body := fmt.Sprintf("%s %s%s %s\n数量 %d\n时间 %s", label, e.Name, season, e.TMDB, p.Count, now())
		if err := bus.EmitData(ctx, tx, "plugin_message", receipt, "media-sync-delete:"+receipt, "【Emby同步删除任务完成】", body, map[string]any{"plugin": Plugin, "receipt": receipt, "count": p.Count, "image": p.Image}); err != nil {
			return err
		}
	}
	return nil
}
func SafeImage(raw string) bool {
	return raw == "https://emby.media/notificationicon.png" || strings.HasPrefix(raw, "https://image.tmdb.org/t/p/") && !strings.ContainsAny(raw, "?#@\\\r\n") && !strings.Contains(raw, "..")
}
func (s *Store) Ignore(ctx context.Context, id, actor, reason string, validate func() error) error {
	if actor == "" || strings.TrimSpace(reason) == "" || !validText(reason, 1000) || validate == nil {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := ReadReceipt(ctx, tx, id)
	if err != nil || p.State == "completed" || p.State == "ignored" || validate() != nil {
		return ErrState
	}
	if p.Parent != "" {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, p.Parent).Scan(&state); err != nil {
			return err
		}
		if state == "completed" {
			return ErrState
		}
		res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REMOVALS SET STATE='closed',UPDATED=?,REASON='original media callback explicitly ignored; retained all prior file facts' WHERE ID=? AND STATE!='completed'`, now(), p.Parent)
		if err != nil {
			return err
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return ErrState
		}
		if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, p.Parent).Scan(&state); err != nil || state != "closed" {
			return ErrState
		}
	}
	raw, err := Setting(ctx, tx, ControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeControl(raw)
	if err != nil || c.Epoch == "" {
		return ErrState
	}
	c.Authorized = false
	if err := WriteControl(ctx, tx, c); err != nil {
		return err
	}
	if err := UpdateState(ctx, tx, p, "ignored", reason, actor); err != nil {
		return err
	}
	if validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
