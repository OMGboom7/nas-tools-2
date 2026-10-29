package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"path"
	"sort"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/movielike"
)

// Favorite reclassification moves an existing complete directory. It is not a
// new intake, history repair, deletion, or inferred successful old transfer.
type FavoriteMoveDefinition struct {
	Receipt        string
	Scope          RecoveryScope
	Source, Target string
	Aliases        []string
	Nodes          []RecoveryNode
	Owners         []RecoveryOwner
}
type FavoriteCopyNode struct {
	Path string
	Node RecoveryNode
	Done bool
}
type FavoriteMoveProof struct {
	Method                     string
	SourceParent, TargetParent string
	Stage                      string
	StageIdentity              string
	Output                     []RecoveryNode
	Copies                     []FavoriteCopyNode
	DeleteIntent               string
	Deleted                    []string
	Frozen                     []RecoveryOwner
}
type FavoriteMoveReview struct {
	ID          string                 `json:"id"`
	State       string                 `json:"state"`
	Phase       string                 `json:"phase"`
	RootID      string                 `json:"rootId"`
	Source      string                 `json:"source"`
	Target      string                 `json:"target"`
	Files       int                    `json:"files"`
	Directories int                    `json:"directories"`
	Links       int                    `json:"links"`
	Bytes       int64                  `json:"bytes"`
	Sample      []string               `json:"sample"`
	Owners      []RecoveryOwner        `json:"owners"`
	Fingerprint string                 `json:"fingerprint"`
	CanContinue bool                   `json:"canContinue"`
	Evidence    string                 `json:"evidence"`
	Definition  FavoriteMoveDefinition `json:"-"`
	Proof       FavoriteMoveProof      `json:"-"`
	Receipt     movielike.Receipt      `json:"-"`
}

func (s *Store) MovieLike() *movielike.Store { return movielike.New(s.db) }
func favoriteWork(p movielike.Receipt) (FavoriteMoveDefinition, FavoriteMoveProof, error) {
	d := FavoriteMoveDefinition{}
	proof := FavoriteMoveProof{}
	if len(p.Work) == 0 || json.Unmarshal(p.Work, &d) != nil || d.Receipt != p.ID || !recoveryScopeValid(d.Scope) || d.Scope.Remote != nil || !validRelative(d.Source) || !validRelative(d.Target) || len(stringsParts(d.Source)) != 2 || len(stringsParts(d.Target)) != 2 || path.Base(d.Source) != path.Base(d.Target) || d.Source == d.Target || len(d.Nodes) < 1 || len(d.Nodes) > RecoveryObjectLimit || d.Nodes[0].Path != "." || d.Nodes[0].Kind != "directory" {
		return d, proof, ErrState
	}
	if len(p.Proof) > 0 && json.Unmarshal(p.Proof, &proof) != nil {
		return d, proof, ErrState
	}
	if len(d.Aliases) > 256 {
		return d, proof, ErrLimit
	}
	for _, alias := range d.Aliases {
		if !path.IsAbs(alias) || path.Clean(alias) != alias || alias == "/" || !movielike.Text(alias, 4096) {
			return d, proof, ErrPath
		}
	}
	if len(proof.Copies) > RecoveryObjectLimit || len(proof.Output) > RecoveryObjectLimit || len(proof.Deleted) > RecoveryObjectLimit {
		return d, proof, ErrLimit
	}
	return d, proof, nil
}
func stringsParts(s string) []string {
	out := []string{}
	for s != "." && s != "" {
		out = append(out, path.Base(s))
		s = path.Dir(s)
	}
	return out
}
func favoriteLocations(d FavoriteMoveDefinition) []string {
	locations := []string{}
	for _, base := range append([]string{d.Scope.Root.Path}, d.Aliases...) {
		for _, relative := range []string{d.Source, d.Target, path.Join(path.Dir(d.Source), ".nastool-move-"+d.Receipt[:32]), path.Join(path.Dir(d.Target), ".nastool-copy-"+d.Receipt[:32])} {
			locations = append(locations, path.Join(base, relative))
		}
	}
	return locations
}
func favoriteClaims(ctx context.Context, r recoveryReader, locations []string, except string) error {
	yes, e := recoveryTable(ctx, r, "GO_MOVIE_LIKE_EVENTS")
	if e != nil || !yes {
		return e
	}
	rows, e := recoveryRows(ctx, r, `SELECT ID FROM GO_MOVIE_LIKE_EVENTS WHERE WORK!='' ORDER BY ID`)
	if e != nil {
		return e
	}
	for _, row := range rows {
		if row[0] == except {
			continue
		}
		p, e := movielike.Read(ctx, r, row[0])
		if e != nil {
			return e
		}
		d, _, e := favoriteWork(p)
		if e != nil {
			return e
		}
		// Completed directories retain no private staging and no old execution
		// authority. Every incomplete/ignored original still protects its objects.
		if p.State == "completed" {
			continue
		}
		for _, a := range favoriteLocations(d) {
			for _, b := range locations {
				if recoveryOverlaps(a, b) {
					return ErrClaimed
				}
			}
		}
	}
	return nil
}
func favoriteOwners(ctx context.Context, r recoveryReader, d FavoriteMoveDefinition) ([]RecoveryOwner, error) {
	// Reuse the complete connected old owner selection, including whole legacy
	// batches; no synthetic removal or intake is ever saved.
	units := []MediaRemovalUnit{}
	for _, base := range append([]string{d.Scope.Root.Path}, d.Aliases...) {
		scope := d.Scope
		scope.Root.Path = base
		for _, loc := range []string{d.Source, d.Target} {
			units = append(units, MediaRemovalUnit{Definition: RecoveryDefinition{Scope: scope, Path: loc}})
		}
	}
	return mediaRemovalOwners(ctx, r, MediaRemovalDefinition{Units: units})
}
func favoriteOtherClaims(ctx context.Context, r recoveryReader, d FavoriteMoveDefinition) error {
	locations := favoriteLocations(d)
	if e := favoriteClaims(ctx, r, locations, d.Receipt); e != nil {
		return e
	}
	if e := mediaRemovalClaimed(ctx, r, locations, nil, d.Receipt); e != nil {
		return e
	}
	yes, e := recoveryTable(ctx, r, "GO_RECOVERY_DISPOSALS")
	if e != nil || !yes {
		return e
	}
	rows, e := recoveryRows(ctx, r, `SELECT ID,DEFINITION FROM GO_RECOVERY_DISPOSALS WHERE STATE NOT IN('completed','returned','closed')`)
	if e != nil {
		return e
	}
	for _, row := range rows {
		var other RecoveryDefinition
		if json.Unmarshal([]byte(row[1]), &other) != nil {
			return ErrState
		}
		for _, a := range locations {
			for _, b := range []string{path.Join(other.Scope.Root.Path, other.Path), path.Join(other.Scope.Root.Path, recoveryCapture(row[0], other.Path))} {
				if recoveryOverlaps(a, b) {
					return ErrClaimed
				}
			}
		}
	}
	return nil
}
func (s *Store) PrepareFavoriteMove(ctx context.Context, p movielike.Receipt, scope RecoveryScope, source, target string, aliases ...string) (FavoriteMoveReview, error) {
	d := FavoriteMoveDefinition{Receipt: p.ID, Scope: scope, Source: source, Target: target, Aliases: aliases}
	approved := false
	for _, root := range p.Definition.Control.Roots {
		if root.ID == scope.Root.ID && root.Path == scope.Root.Path && root.Identity == scope.Root.Identity {
			approved = true
		}
	}
	category := false
	for _, name := range p.Definition.Control.Categories {
		if name == path.Dir(source) {
			category = true
		}
	}
	if !approved || !category || scope.ConfigDigest != p.Definition.Control.AppDigest || path.Dir(target) != p.Definition.Spec.Directory {
		return FavoriteMoveReview{}, ErrState
	}
	if len(aliases) > 256 {
		return FavoriteMoveReview{}, ErrLimit
	}
	for _, alias := range aliases {
		if !path.IsAbs(alias) || path.Clean(alias) != alias || alias == "/" || !movielike.Text(alias, 4096) {
			return FavoriteMoveReview{}, ErrPath
		}
	}
	if p.State != "pending" || !movielike.Hex(p.ID, 64) || scope.Remote != nil || !recoveryScopeValid(scope) || !validRelative(source) || !validRelative(target) || len(stringsParts(source)) != 2 || len(stringsParts(target)) != 2 || path.Base(source) != path.Base(target) || source == target {
		return FavoriteMoveReview{}, ErrPath
	}
	nodes, e := favoriteSnapshot(ctx, d, source)
	if e != nil {
		return FavoriteMoveReview{}, e
	}
	if len(nodes) == 0 || nodes[0].Kind != "directory" {
		return FavoriteMoveReview{}, ErrPath
	}
	d.Nodes = nodes
	exists, e := favoritePresence(d, target)
	if e != nil || exists {
		return FavoriteMoveReview{}, ErrClaimed
	}
	d.Owners, e = favoriteOwners(ctx, s.db, d)
	if e != nil {
		return FavoriteMoveReview{}, e
	}
	if e := favoriteOtherClaims(ctx, s.db, d); e != nil {
		return FavoriteMoveReview{}, e
	}
	return favoriteProject(p, d, FavoriteMoveProof{}, "whole original directory read; no filesystem changes", true), nil
}
func favoriteProject(p movielike.Receipt, d FavoriteMoveDefinition, proof FavoriteMoveProof, evidence string, can bool) FavoriteMoveReview {
	v := FavoriteMoveReview{ID: p.ID, State: p.State, Phase: p.Phase, RootID: d.Scope.Root.ID, Source: d.Source, Target: d.Target, Sample: []string{}, Owners: d.Owners, Evidence: evidence, CanContinue: can, Definition: d, Proof: proof, Receipt: p}
	for _, n := range d.Nodes {
		switch n.Kind {
		case "directory":
			v.Directories++
		case "link":
			v.Links++
		case "file":
			v.Files++
			v.Bytes += n.Node.Size
		}
		if len(v.Sample) < 100 {
			v.Sample = append(v.Sample, n.Path)
		}
	}
	v.Fingerprint = Digest([]any{p.ID, p.State, p.Phase, p.Updated, d, proof, evidence, can})
	return v
}
func (s *Store) SaveFavoriteMove(ctx context.Context, v FavoriteMoveReview, validate func() error) error {
	if validate == nil || v.Receipt.State != "pending" || validate() != nil {
		return ErrState
	}
	d := v.Definition
	raw, _ := json.Marshal(d)
	return s.MovieLike().Transaction(ctx, func(tx *sql.Tx) error {
		p, e := movielike.Read(ctx, tx, v.ID)
		if e != nil || p.State != "pending" || p.Updated != v.Receipt.Updated || movielike.CheckAutomatic(ctx, tx, p) != nil || validate() != nil {
			return ErrState
		}
		owners, e := favoriteOwners(ctx, tx, d)
		if e != nil || Digest(owners) != Digest(d.Owners) {
			return ErrState
		}
		if e := favoriteOtherClaims(ctx, tx, d); e != nil {
			return e
		}
		if e := movielike.Advance(ctx, tx, p, "planned", "planned", "完整原目录和关联范围已冻结，尚未移动", "", raw, nil); e != nil {
			return e
		}
		if movielike.CheckAutomatic(ctx, tx, p) != nil || validate() != nil {
			return ErrState
		}
		return nil
	})
}
func (s *Store) ReviewFavoriteMove(ctx context.Context, id string) (FavoriteMoveReview, error) {
	p, e := s.MovieLike().Receipt(ctx, id)
	if e != nil {
		return FavoriteMoveReview{}, e
	}
	d, proof, e := favoriteWork(p)
	if e != nil {
		return FavoriteMoveReview{}, e
	}
	if p.State == "completed" {
		return favoriteProject(p, d, proof, "已提交原完整目录移动及刷新事件，不重新执行", false), nil
	}
	if p.State == "ignored" {
		return favoriteProject(p, d, proof, "原任务已停止，原范围和未完成对象继续保留", false), nil
	}
	owners, e := favoriteOwners(ctx, s.db, d)
	if e != nil {
		return FavoriteMoveReview{}, e
	}
	expected := d.Owners
	if proof.Frozen != nil {
		expected = proof.Frozen
	}
	can := Digest(owners) == Digest(expected)
	evidence := "完整原内容及已有阶段回执已核实"
	if !can {
		evidence = "原关联任务或内容范围变化，不能继续"
	}
	if e := favoriteOtherClaims(ctx, s.db, d); e != nil {
		can = false
		evidence = "其他删除／移动范围占用未解除"
	}
	if e := favoriteReviewFiles(ctx, d, proof, p.Phase); e != nil {
		can = false
		evidence = "原文件、目标、暂存或捕获内容未核实，保留全部现有对象"
	}
	return favoriteProject(p, d, proof, evidence, can), nil
}
func (s *Store) favoriteSave(ctx context.Context, id, state, phase, reason, actor string, proof FavoriteMoveProof, current func() error) error {
	raw, _ := json.Marshal(proof)
	return s.MovieLike().Transaction(ctx, func(tx *sql.Tx) error {
		p, e := movielike.Read(ctx, tx, id)
		if e != nil || p.State == "completed" || p.State == "ignored" || p.State == "blocked" {
			return ErrState
		}
		if current != nil {
			if e := current(); e != nil {
				return e
			}
		}
		if e := movielike.Advance(ctx, tx, p, state, phase, reason, actor, nil, raw); e != nil {
			return e
		}
		if current != nil {
			return current()
		}
		return nil
	})
}
func (s *Store) ExecuteFavoriteMove(ctx context.Context, v FavoriteMoveReview, actor string, automatic bool, validate func() error) (err error) {
	if validate == nil || !v.CanContinue || v.ID == "" || actor == "" {
		return ErrState
	}
	currentReview, e := s.ReviewFavoriteMove(ctx, v.ID)
	if e != nil || currentReview.Fingerprint != v.Fingerprint || !currentReview.CanContinue {
		return ErrState
	}
	d, proof, p := v.Definition, v.Proof, v.Receipt
	err = s.MovieLike().Transaction(ctx, func(tx *sql.Tx) error {
		actual, e := movielike.Read(ctx, tx, p.ID)
		if e != nil || actual.Updated != p.Updated || validate() != nil {
			return ErrState
		}
		if automatic && movielike.CheckAutomatic(ctx, tx, p) != nil {
			return ErrState
		}
		owners, e := favoriteOwners(ctx, tx, d)
		if e != nil {
			return e
		}
		expected := d.Owners
		if proof.Frozen != nil {
			expected = proof.Frozen
		}
		if Digest(owners) != Digest(expected) {
			return ErrState
		}
		if e := favoriteOtherClaims(ctx, tx, d); e != nil {
			return e
		}
		if proof.Frozen == nil {
			if e := freezeRecoveryOwners(ctx, tx, d.Owners, p.ID); e != nil {
				return e
			}
			proof.Frozen, e = favoriteOwners(ctx, tx, d)
			if e != nil {
				return e
			}
		}
		b, _ := json.Marshal(proof)
		if e := movielike.Advance(ctx, tx, p, "running", p.Phase, "已保存这次完整目录移动意图，重启不自动续跑", actor, nil, b); e != nil {
			return e
		}
		if automatic && movielike.CheckAutomatic(ctx, tx, p) != nil {
			return ErrState
		}
		return validate()
	})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5e9)
			defer cancel()
			_ = s.favoriteSave(persist, p.ID, "needs_review", p.Phase, "本次移动中断，已有事实和剩余对象保留，不自动续跑", actor, proof, nil)
		}
	}()
	current := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := validate(); e != nil {
			return e
		}
		now, e := s.MovieLike().Receipt(ctx, p.ID)
		if e != nil || now.State != "running" {
			return ErrState
		}
		if automatic {
			if e := movielike.CheckAutomatic(ctx, s.db, now); e != nil {
				return e
			}
		}
		owners, e := favoriteOwners(ctx, s.db, d)
		if e != nil || Digest(owners) != Digest(proof.Frozen) {
			return ErrState
		}
		return favoriteOtherClaims(ctx, s.db, d)
	}
	save := func(phase string) error {
		if e := s.favoriteSave(ctx, p.ID, "running", phase, "原目录移动阶段和完整凭证已保存", actor, proof, validate); e != nil {
			return e
		}
		p.Phase = phase
		return current()
	}
	if err = favoriteExecute(ctx, d, &proof, &p.Phase, current, save); err != nil {
		return err
	}
	if err = current(); err != nil {
		return err
	}
	err = s.MovieLike().Transaction(ctx, func(tx *sql.Tx) error {
		actual, e := movielike.Read(ctx, tx, p.ID)
		if e != nil || actual.State != "running" || validate() != nil {
			return ErrState
		}
		if automatic && movielike.CheckAutomatic(ctx, tx, actual) != nil {
			return ErrState
		}
		owners, e := favoriteOwners(ctx, tx, d)
		if e != nil || Digest(owners) != Digest(proof.Frozen) {
			return ErrState
		}
		b, _ := json.Marshal(proof)
		if e := movielike.Advance(ctx, tx, actual, "completed", "completed", "完整原目录已移动，原历史不改；刷新事件同事务提交", actor, nil, b); e != nil {
			return e
		}
		dest := path.Join(d.Scope.Root.Path, d.Target)
		if e := bus.PublishSignal(ctx, tx, "refresh.mediaserver", p.ID, "movie-like:"+p.ID, map[string]any{"dest": dest, "media_info": map[string]any{}}); e != nil {
			return e
		}
		if e := mediaserver.EnqueueFavoriteRefresh(ctx, tx, p.ID); e != nil {
			return e
		}
		if automatic && movielike.CheckAutomatic(ctx, tx, actual) != nil {
			return ErrState
		}
		return validate()
	})
	return err
}
func (s *Store) IgnoreFavoriteMove(ctx context.Context, id, actor, reason string, validate func() error) error {
	if actor == "" || reason == "" || !movielike.Text(reason, 1000) || validate == nil {
		return ErrState
	}
	return s.MovieLike().Transaction(ctx, func(tx *sql.Tx) error {
		p, e := movielike.Read(ctx, tx, id)
		if e != nil || p.State == "completed" || p.State == "ignored" || validate() != nil {
			return ErrState
		}
		raw, e := movielike.Setting(ctx, tx, movielike.ControlKey)
		if e != nil {
			return e
		}
		c, e := movielike.DecodeControl(raw)
		if e != nil || c.Epoch == "" {
			return ErrState
		}
		c.Authorized = false
		if e := movielike.WriteControl(ctx, tx, c); e != nil {
			return e
		}
		if e := movielike.Advance(ctx, tx, p, "ignored", p.Phase, reason, actor, nil, nil); e != nil {
			return e
		}
		return validate()
	})
}
func favoriteContent(nodes []RecoveryNode) string {
	type content struct {
		Path, Kind, Digest, Link string
		Size                     int64
		Mode                     uint32
	}
	values := []content{}
	for _, n := range nodes {
		size := n.Node.Size
		if n.Kind != "file" {
			size = 0
		}
		values = append(values, content{n.Path, n.Kind, n.Digest, n.Link, size, n.Mode})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Path < values[j].Path })
	return Digest(values)
}
func favoriteExact(a, b []RecoveryNode) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	left, right := append([]RecoveryNode{}, a...), append([]RecoveryNode{}, b...)
	// The complete directory keeps its identity across a rename; its immediate
	// parent legitimately changes. Both parent bindings are checked separately.
	if left[0].Path == "." && right[0].Path == "." {
		left[0].Node.Parent = ""
		right[0].Node.Parent = ""
	}
	return Digest(left) == Digest(right)
}
