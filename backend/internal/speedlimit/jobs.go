package speedlimit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

type TargetPlan struct {
	Target        Target
	Before, After Rates
}
type OperationPlan struct {
	Target        Target
	Dimension     string
	Before, After Rates
}
type Definition struct {
	Version      int
	ID           string
	Trigger      Trigger
	TriggerEpoch string
	Control      Control
	Observation  Observation
	Notify       bool
	Smart        bool
	Targets      []TargetPlan
	Operations   []OperationPlan
}
type Operation struct {
	Ordinal   int    `json:"ordinal"`
	TargetID  string `json:"targetId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Dimension string `json:"dimension"`
	Before    Rates  `json:"before"`
	After     Rates  `json:"after"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
	Updated   string `json:"updated"`
}
type Job struct {
	ID          string      `json:"id"`
	Source      string      `json:"source"`
	Reference   string      `json:"reference"`
	Observation Observation `json:"observation"`
	State       string      `json:"state"`
	Created     string      `json:"created"`
	Updated     string      `json:"updated"`
	Operations  []Operation `json:"operations"`
	Definition  Definition  `json:"-"`
	Fingerprint string      `json:"-"`
}

func plan(c Control, spec Spec, p Trigger, observation Observation, before []Baseline) (Definition, error) {
	d := Definition{Version: 1, ID: Digest([]string{"speed-limiter", p.ID}), Trigger: p, TriggerEpoch: p.Epoch, Control: c, Observation: observation, Notify: spec.Notify, Smart: spec.Bandwidth > 0, Targets: []TargetPlan{}, Operations: []OperationPlan{}}
	if !c.Authorized || p.Epoch != c.Epoch || len(before) != len(c.Baselines) {
		return d, ErrState
	}
	targets := []Target{}
	for i, b := range before {
		if b.Target != c.Baselines[i].Target || !b.Rates.Valid(b.Target.Kind) {
			return d, ErrState
		}
		targets = append(targets, b.Target)
	}
	limits, err := spec.Limits(observation, targets)
	if err != nil {
		return d, err
	}
	for i, b := range before {
		after, err := b.Rates.Desired(b.Target.Kind, limits[i][0], limits[i][1])
		if err != nil {
			return d, err
		}
		d.Targets = append(d.Targets, TargetPlan{b.Target, b.Rates, after})
		if b.Target.Kind == "transmission" {
			if b.Rates != after {
				d.Operations = append(d.Operations, OperationPlan{b.Target, "pair", b.Rates, after})
			}
			continue
		}
		current := b.Rates
		if current.Upload != after.Upload {
			next := current
			next.Upload = after.Upload
			next.UploadLimited = after.UploadLimited
			d.Operations = append(d.Operations, OperationPlan{b.Target, "upload", current, next})
			current = next
		}
		if current.Download != after.Download {
			d.Operations = append(d.Operations, OperationPlan{b.Target, "download", current, after})
		}
	}
	return d, nil
}
func (s *Store) CreateJob(ctx context.Context, c Control, spec Spec, p Trigger, o Observation, before []Baseline, validate func() error) (Job, error) {
	d, err := plan(c, spec, p, o, before)
	if err != nil {
		return Job{}, err
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 1<<20 {
		return Job{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	current, currentSpec, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(current) != Digest(c) || Digest(currentSpec) != Digest(spec) {
		return Job{}, ErrState
	}
	if err := validate(); err != nil {
		return Job{}, err
	}
	for _, b := range before {
		owned, err := owner(ctx, tx, c.Epoch, b.Target)
		if err != nil || owned != b.Rates {
			return Job{}, ErrState
		}
		if yes, err := unresolved(ctx, tx, b.Target.ID); err != nil || yes {
			return Job{}, ErrState
		}
	}
	now := nowString()
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_JOBS VALUES(?,?,?,?,?)`, d.ID, string(raw), Digest(d), now, now)
	if err != nil {
		return Job{}, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return Job{}, ErrState
	}
	for i, op := range d.Operations {
		res, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_OPERATIONS VALUES(?,?,?,?,'pending','','',?)`, d.ID, i, c.Epoch, op.Target.ID, now)
		if err != nil {
			return Job{}, err
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return Job{}, ErrState
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_TRIGGERS SET STATE='planned' WHERE ID=? AND EPOCH=? AND STATE='pending'`, p.ID, c.Epoch)
	if err != nil {
		return Job{}, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return Job{}, ErrState
	}
	saved, err := readJob(ctx, tx, d.ID)
	if err != nil || saved.Fingerprint != Digest(d) || len(saved.Operations) != len(d.Operations) {
		return Job{}, ErrState
	}
	current, _, active, err = readScope(ctx, tx)
	if err != nil || !active || Digest(current) != Digest(c) {
		return Job{}, ErrState
	}
	if err := validate(); err != nil {
		return Job{}, err
	}
	return saved, tx.Commit()
}
func readJob(ctx context.Context, r Reader, id string) (Job, error) {
	j := Job{}
	if !Hex(id, 64) {
		return j, ErrMissing
	}
	yes, err := table(ctx, r, "GO_SPEED_LIMITER_JOBS")
	if err != nil {
		return j, err
	}
	if !yes {
		return j, ErrMissing
	}
	var raw string
	err = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,FP,CREATED,UPDATED FROM GO_SPEED_LIMITER_JOBS WHERE ID=?`, id).Scan(&j.ID, &raw, &j.Fingerprint, &j.Created, &j.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrMissing
	}
	if err != nil {
		return j, err
	}
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &j.Definition) != nil || j.ID != id || j.Definition.ID != id || j.Definition.Version != 1 || Digest(j.Definition) != j.Fingerprint || id != Digest([]string{"speed-limiter", j.Definition.Trigger.ID}) || len(j.Definition.Operations) > 128 {
		return j, ErrState
	}
	controlRaw, _ := json.Marshal(j.Definition.Control)
	if _, err := DecodeControl(string(controlRaw)); err != nil {
		return j, err
	}
	if !j.Definition.Control.Authorized || j.Definition.TriggerEpoch != j.Definition.Control.Epoch || !Hex(j.Definition.Trigger.ID, 64) || len(j.Definition.Targets) != len(j.Definition.Control.Baselines) {
		return j, ErrState
	}
	if j.Definition.Trigger.Source != "timer" && j.Definition.Trigger.Source != "webhook" {
		return j, ErrState
	}
	targets := map[string]Target{}
	for i, p := range j.Definition.Targets {
		if p.Target != j.Definition.Control.Baselines[i].Target || !p.Before.Valid(p.Target.Kind) || !p.After.Valid(p.Target.Kind) {
			return j, ErrState
		}
		targets[p.Target.ID] = p.Target
	}
	for _, p := range j.Definition.Operations {
		if targets[p.Target.ID] != p.Target || p.Target.Kind == "transmission" && p.Dimension != "pair" || p.Target.Kind == "qbittorrent" && p.Dimension != "upload" && p.Dimension != "download" {
			return j, ErrState
		}
	}
	j.Source, j.Reference, j.Observation = j.Definition.Trigger.Source, j.Definition.Trigger.Reference, j.Definition.Observation
	j.State = "not_finalized"
	err = r.QueryRowContext(ctx, `SELECT STATE FROM GO_SPEED_LIMITER_JOB_FINALS WHERE JOB_ID=?`, id).Scan(&j.State)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return j, err
	}
	switch j.State {
	case "not_finalized", "confirmed", "partial", "stopped", "ignored":
	default:
		return j, ErrState
	}
	j.Operations = []Operation{}
	rows, err := r.QueryContext(ctx, `SELECT ORDINAL,TARGET_ID,STATE,REASON,ACTOR,UPDATED FROM GO_SPEED_LIMITER_OPERATIONS WHERE JOB_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		var op Operation
		if err := rows.Scan(&op.Ordinal, &op.TargetID, &op.State, &op.Reason, &op.Actor, &op.Updated); err != nil {
			return j, err
		}
		if op.Ordinal != len(j.Operations) || op.Ordinal >= len(j.Definition.Operations) {
			return j, ErrState
		}
		p := j.Definition.Operations[op.Ordinal]
		if p.Target.ID != op.TargetID || !p.Before.Valid(p.Target.Kind) || !p.After.Valid(p.Target.Kind) {
			return j, ErrState
		}
		op.Name, op.Kind, op.Dimension, op.Before, op.After = p.Target.Name, p.Target.Kind, p.Dimension, p.Before, p.After
		switch op.State {
		case "pending", "intent", "confirmed", "verified_current", "uncertain", "blocked", "ignored":
		default:
			return j, ErrState
		}
		j.Operations = append(j.Operations, op)
	}
	if err := rows.Err(); err != nil {
		return j, err
	}
	if len(j.Operations) != len(j.Definition.Operations) {
		return j, ErrState
	}
	return j, nil
}
func (s *Store) Job(ctx context.Context, id string) (Job, error) { return readJob(ctx, s.db, id) }
func (s *Store) Jobs(ctx context.Context, page int) ([]Job, error) {
	out := []Job{}
	if page < 1 || page > 10000 {
		return out, ErrState
	}
	yes, err := table(ctx, s.db, "GO_SPEED_LIMITER_JOBS")
	if err != nil || !yes {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_SPEED_LIMITER_JOBS ORDER BY CREATED DESC,ID DESC LIMIT 30 OFFSET ?`, (page-1)*30)
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
		j, err := readJob(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func operationState(ctx context.Context, tx *sql.Tx, j Job, index int, from, state, reason, actor string) error {
	res, err := tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_OPERATIONS SET STATE=?,REASON=?,ACTOR=?,UPDATED=? WHERE JOB_ID=? AND ORDINAL=? AND STATE=?`, state, reason, actor, nowString(), j.ID, index, from)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var saved, why, by string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON,ACTOR FROM GO_SPEED_LIMITER_OPERATIONS WHERE JOB_ID=? AND ORDINAL=?`, j.ID, index).Scan(&saved, &why, &by); err != nil || saved != state || why != reason || by != actor {
		return ErrState
	}
	return nil
}
func (s *Store) BeginOperation(ctx context.Context, expected Job, index int, validate func() error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readJob(ctx, tx, expected.ID)
	if err != nil || j.Fingerprint != expected.Fingerprint || index < 0 || index >= len(j.Operations) || j.Operations[index].State != "pending" {
		return ErrState
	}
	p := j.Definition.Operations[index]
	c, _, active, scopeErr := readScope(ctx, tx)
	valid := scopeErr == nil && active && Digest(c) == Digest(j.Definition.Control) && validate != nil && validate() == nil
	owned, e := owner(ctx, tx, j.Definition.Control.Epoch, p.Target)
	valid = valid && e == nil && owned == p.Before
	if yes, e := unresolved(ctx, tx, p.Target.ID); e != nil || yes {
		valid = false
	}
	if !valid {
		if err := operationState(ctx, tx, j, index, "pending", "blocked", "original limiter authority, rate ownership or connection changed", ""); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrState
	}
	if err := operationState(ctx, tx, j, index, "pending", "intent", "", ""); err != nil {
		return err
	}
	final, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(final) != Digest(c) || validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) BlockOperation(ctx context.Context, j Job, index int, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readJob(ctx, tx, j.ID)
	if err != nil || current.Fingerprint != j.Fingerprint || index < 0 || index >= len(current.Operations) {
		return ErrState
	}
	if err := operationState(ctx, tx, current, index, "pending", "blocked", reason, ""); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FinishOperation(ctx context.Context, j Job, index int, confirmed bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readJob(ctx, tx, j.ID)
	if err != nil || current.Fingerprint != j.Fingerprint || index < 0 || index >= len(current.Operations) || current.Operations[index].State != "intent" {
		return ErrState
	}
	state, reason := "uncertain", "original rate mutation or read-back not confirmed; no automatic retry"
	if confirmed {
		state, reason = "confirmed", "observed complete original rate snapshot after acknowledged mutation"
		p := current.Definition.Operations[index]
		if err := saveOwner(ctx, tx, current.Definition.Control, p.Target, p.Before, p.After); err != nil {
			return err
		}
	}
	if err := operationState(ctx, tx, current, index, "intent", state, reason, ""); err != nil {
		return err
	}
	saved, err := readJob(ctx, tx, j.ID)
	if err != nil || saved.Fingerprint != j.Fingerprint || saved.Operations[index].State != state {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) VerifyOperation(ctx context.Context, j Job, index int, observed Rates, actor string, validate func() error) error {
	if actor == "" || validate == nil {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readJob(ctx, tx, j.ID)
	if err != nil || current.Fingerprint != j.Fingerprint || index < 0 || index >= len(current.Operations) {
		return ErrState
	}
	op := current.Operations[index]
	p := current.Definition.Operations[index]
	if op.State != "intent" && op.State != "uncertain" || observed != p.After || validate() != nil {
		return ErrState
	}
	if _, err := targetConfig(ctx, tx, p.Target); err != nil {
		return err
	}
	if err := saveOwner(ctx, tx, current.Definition.Control, p.Target, p.Before, p.After); err != nil {
		return err
	}
	if err := operationState(ctx, tx, current, index, op.State, "verified_current", "administrator read the exact original resulting snapshot; this does not identify who changed it", actor); err != nil {
		return err
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Finalize(ctx context.Context, j Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readJob(ctx, tx, j.ID)
	if err != nil || current.Fingerprint != j.Fingerprint {
		return ErrState
	}
	if current.State != "not_finalized" {
		return nil
	}
	state := "confirmed"
	for i, op := range current.Operations {
		if op.State == "pending" {
			if err := operationState(ctx, tx, current, i, "pending", "blocked", "this run ended before dispatch; it will not resume automatically", ""); err != nil {
				return err
			}
			state = "partial"
		} else if op.State != "confirmed" && op.State != "verified_current" {
			state = "partial"
		}
	}
	c, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(c) != Digest(current.Definition.Control) {
		state = "stopped"
	}
	now := nowString()
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_JOB_FINALS VALUES(?,?,?)`, j.ID, state, now)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var saved string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_SPEED_LIMITER_JOB_FINALS WHERE JOB_ID=?`, j.ID).Scan(&saved); err != nil || saved != state {
		return ErrState
	}
	if state == "confirmed" {
		var previous int
		if err := tx.QueryRowContext(ctx, `SELECT PLAYING FROM GO_SPEED_LIMITER_PLAYSTATE WHERE EPOCH=?`, c.Epoch).Scan(&previous); err != nil {
			return err
		}
		playing := current.Observation.Playing > 0
		if current.Definition.Notify && (playing != (previous != 0) || current.Source == "webhook" && current.Definition.Smart && playing) {
			title := "下载器未播放限速已核对"
			if playing {
				title = "下载器播放限速已核对"
			}
			lines := []string{}
			for _, p := range current.Definition.Targets {
				lines = append(lines, fmt.Sprintf("%s：上传 %g、下载 %g %s", p.Target.Name, p.After.Upload, p.After.Download, p.After.Unit))
			}
			if err := bus.EmitData(ctx, tx, "plugin_message", j.ID, "speed-limiter:"+j.ID, title, strings.Join(lines, "\n"), map[string]any{"plugin": Plugin, "job_id": j.ID, "playing": playing}); err != nil {
				return err
			}
		}
		n := 0
		if playing {
			n = 1
		}
		res, err := tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_PLAYSTATE SET PLAYING=? WHERE EPOCH=?`, n, c.Epoch)
		if err != nil {
			return err
		}
		if count, e := res.RowsAffected(); e != nil || count != 1 {
			return ErrState
		}
		var final int
		if err := tx.QueryRowContext(ctx, `SELECT PLAYING FROM GO_SPEED_LIMITER_PLAYSTATE WHERE EPOCH=?`, c.Epoch).Scan(&final); err != nil || final != n {
			return ErrState
		}
	}
	return tx.Commit()
}
func (s *Store) IgnoreJob(ctx context.Context, j Job, actor, reason string, validate func() error) error {
	if actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 1000 || strings.ContainsRune(reason, '\x00') || validate == nil {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readJob(ctx, tx, j.ID)
	if err != nil || current.Fingerprint != j.Fingerprint {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	changed := false
	for i, op := range current.Operations {
		if op.State == "confirmed" || op.State == "verified_current" || op.State == "ignored" {
			continue
		}
		if err := operationState(ctx, tx, current, i, op.State, "ignored", reason, actor); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return ErrState
	}
	raw, err := setting(ctx, tx, ControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeControl(raw)
	if err != nil || c.Epoch == "" {
		return ErrState
	}
	c.Authorized = false
	if err := setControl(ctx, tx, c); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_JOB_FINALS VALUES(?,'ignored',?) ON CONFLICT(JOB_ID) DO UPDATE SET STATE='ignored'`, j.ID, nowString()); err != nil {
		return err
	}
	saved, err := readJob(ctx, tx, j.ID)
	if err != nil || saved.State != "ignored" {
		return ErrState
	}
	for _, op := range saved.Operations {
		if op.State != "confirmed" && op.State != "verified_current" && op.State != "ignored" {
			return ErrState
		}
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func JobIDForTrigger(p Trigger) string           { return Digest([]string{"speed-limiter", p.ID}) }
func OperationReference(j Job, index int) string { return j.ID + ":" + strconv.Itoa(index) }
