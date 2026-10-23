package organization

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

type MediaRemovalEventRef struct {
	HistoryID string `json:"historyId"`
	Role      string `json:"role"`
	Path      string `json:"path"`
}
type MediaRemovalUnit struct {
	Definition RecoveryDefinition     `json:"definition"`
	Absent     bool                   `json:"absent"`
	Events     []MediaRemovalEventRef `json:"events"`
}
type MediaRemovalDefinition struct {
	Actor       string                        `json:"actor"`
	RequestID   string                        `json:"requestId"`
	Decision    string                        `json:"decision"`
	InputDigest string                        `json:"inputDigest"`
	Histories   []mediaRemovalHistoryDocument `json:"histories"`
	Units       []MediaRemovalUnit            `json:"units"`
	Owners      []RecoveryOwner               `json:"owners"`
}
type MediaRemovalScopeView struct {
	Ordinal     int                     `json:"ordinal"`
	RootID      string                  `json:"rootId"`
	Driver      string                  `json:"driver"`
	Path        string                  `json:"path"`
	Role        string                  `json:"role"`
	Absent      bool                    `json:"absent"`
	Files       int                     `json:"files"`
	Directories int                     `json:"directories"`
	Links       int                     `json:"links"`
	Bytes       int64                   `json:"bytes"`
	Sample      []string                `json:"sample"`
	Events      []MediaRemovalEventRef  `json:"events"`
	Disposal    *RecoveryDisposal       `json:"disposal,omitempty"`
	Review      *RecoveryDisposalReview `json:"review,omitempty"`
}
type MediaRemovalReview struct {
	ID                  string                       `json:"id"`
	State               string                       `json:"state"`
	Actor               string                       `json:"actor"`
	RequestID           string                       `json:"requestId"`
	Decision            string                       `json:"decision"`
	Created             string                       `json:"created"`
	Updated             string                       `json:"updated"`
	Reason              string                       `json:"reason"`
	Fingerprint         string                       `json:"fingerprint"`
	CreationFingerprint string                       `json:"creationFingerprint"`
	CanContinue         bool                         `json:"canContinue"`
	HistoryIDs          []string                     `json:"historyIds"`
	Records             []MediaRemovalHistorySummary `json:"records"`
	DedupeRows          int                          `json:"dedupeRows"`
	Units               []MediaRemovalScopeView      `json:"units"`
	Owners              []RecoveryOwner              `json:"owners"`
	Definition          MediaRemovalDefinition       `json:"-"`
	LastReview          string                       `json:"-"`
	Frozen              []RecoveryOwner              `json:"-"`
}
type MediaRemovalHistorySummary struct {
	HistoryRecord
	ID string `json:"id"`
}

func MediaRemovalID(actor, request string) string {
	return Digest([]string{"media-removal", actor, request})
}
func mediaRemovalChildRequest(parent string, ordinal int) string {
	return Digest([]any{"media-removal-child", parent, ordinal})[:32]
}
func mediaRemovalChildID(d MediaRemovalDefinition, ordinal int) string {
	return RecoveryDisposalID(d.Actor, mediaRemovalChildRequest(MediaRemovalID(d.Actor, d.RequestID), ordinal))
}
func mediaRemovalTerminal(state string) bool { return state == "completed" || state == "closed" }

func MediaRemovalPresence(ctx context.Context, s RecoveryScope, selected string, c *remotestore.Client) (bool, error) {
	if !validRelative(selected) || selected == "." {
		return false, ErrPath
	}
	if err := recoveryClient(s, c); err != nil {
		return false, err
	}
	if s.Remote != nil {
		n, err := c.Entry(ctx, s.Root.Path, selected)
		return n != nil, err
	}
	n, err := recoveryLocalPresence(s, selected)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return n != nil, err
}

func mediaRemovalOwners(ctx context.Context, r recoveryReader, d MediaRemovalDefinition) ([]RecoveryOwner, error) {
	records, err := recoveryOwnerRecords(ctx, r)
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	batches := map[string]bool{}
	histories := map[int64]bool{}
	for _, h := range d.Histories {
		histories[h.Record.ID] = true
	}
	for _, rec := range records {
		match := false
		if rec.kind == "organization" {
			var job Definition
			if json.Unmarshal([]byte(rec.definition), &job) != nil {
				return nil, ErrState
			}
			match = job.History != nil && histories[job.History.ID]
		}
		for _, unit := range d.Units {
			p := path.Join(unit.Definition.Scope.Root.Path, unit.Definition.Path)
			for _, candidate := range append(append([]string{}, rec.paths...), rec.protected...) {
				if recoveryOverlaps(p, candidate) {
					match = true
					break
				}
			}
		}
		if match {
			selected[rec.kind+":"+rec.id] = true
			if rec.batch != "" {
				batches[rec.batch] = true
			}
		}
	}
	owners := []RecoveryOwner{}
	for _, rec := range records {
		direct := selected[rec.kind+":"+rec.id]
		if !direct && !(rec.kind == "organization" && batches[rec.batch]) {
			continue
		}
		owners = append(owners, RecoveryOwner{Kind: rec.kind, ID: rec.id, BatchID: rec.batch, State: rec.state, Related: !direct, Fingerprint: Digest([]any{rec.kind, rec.id, rec.state, rec.definition, rec.rows})})
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].Kind == owners[j].Kind {
			return owners[i].ID < owners[j].ID
		}
		return owners[i].Kind < owners[j].Kind
	})
	return owners, nil
}
func (s *Store) MediaRemovalOwners(ctx context.Context, d MediaRemovalDefinition) ([]RecoveryOwner, error) {
	return mediaRemovalOwners(ctx, s.db, d)
}
func (s *Store) MediaRemovalGuardOwners(ctx context.Context, d MediaRemovalDefinition, ids []int64) ([]RecoveryOwner, error) {
	var err error
	d.Histories, err = mediaRemovalHistories(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	return mediaRemovalOwners(ctx, s.db, d)
}
func (s *Store) MediaRemovalUnitOwners(ctx context.Context, d RecoveryDefinition) ([]RecoveryOwner, error) {
	if d.Removal == nil || !recoveryDefinitionPath(d) {
		return nil, ErrState
	}
	return recoveryDefinitionOwners(ctx, s.db, d)
}

func (s *Store) PrepareMediaRemoval(ctx context.Context, d MediaRemovalDefinition, ids []int64) (MediaRemovalReview, error) {
	if d.Actor == "" || !recoveryHex(d.RequestID, 32) || !recoveryHex(d.InputDigest, 64) || strings.TrimSpace(d.Decision) == "" || len(d.Decision) > 512 || len(d.Units) > 1024 || len(ids) == 0 && len(d.Units) == 0 {
		return MediaRemovalReview{}, ErrState
	}
	var err error
	d.Histories, err = mediaRemovalHistories(ctx, s.db, ids)
	if err != nil {
		return MediaRemovalReview{}, err
	}
	parent := MediaRemovalID(d.Actor, d.RequestID)
	var objects int
	var bytes int64
	for i := range d.Units {
		u := &d.Units[i]
		child := &u.Definition
		child.Actor, child.RequestID, child.Decision = d.Actor, mediaRemovalChildRequest(parent, i), d.Decision
		if child.Removal == nil {
			return MediaRemovalReview{}, ErrState
		}
		child.Removal.Parent = parent
		if !recoveryScopeValid(child.Scope) || !recoveryDefinitionPath(*child) || u.Absent != (len(child.Nodes) == 0) {
			return MediaRemovalReview{}, ErrState
		}
		child.Owners, err = recoveryDefinitionOwners(ctx, s.db, *child)
		if err != nil {
			return MediaRemovalReview{}, err
		}
		for _, n := range child.Nodes {
			objects++
			if n.Kind == "file" {
				if n.Node.Size < 0 || n.Node.Size > RecoveryByteLimit-bytes {
					return MediaRemovalReview{}, ErrLimit
				}
				bytes += n.Node.Size
			}
		}
		for j := 0; j < i; j++ {
			other := d.Units[j].Definition
			if recoveryOverlaps(path.Join(child.Scope.Root.Path, child.Path), path.Join(other.Scope.Root.Path, other.Path)) {
				return MediaRemovalReview{}, ErrPath
			}
		}
	}
	if objects > RecoveryObjectLimit {
		return MediaRemovalReview{}, ErrLimit
	}
	d.Owners, err = mediaRemovalOwners(ctx, s.db, d)
	if err != nil {
		return MediaRemovalReview{}, err
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 64<<20 {
		return MediaRemovalReview{}, ErrLimit
	}
	v := mediaRemovalProject(d)
	v.State = "preview"
	v.Fingerprint = Digest(d)
	v.CanContinue = true
	return v, nil
}
func mediaRemovalProject(d MediaRemovalDefinition) MediaRemovalReview {
	v := MediaRemovalReview{ID: MediaRemovalID(d.Actor, d.RequestID), Actor: d.Actor, RequestID: d.RequestID, Decision: d.Decision, HistoryIDs: []string{}, Records: []MediaRemovalHistorySummary{}, Units: []MediaRemovalScopeView{}, Owners: d.Owners, Definition: d, CreationFingerprint: Digest(d)}
	seen := map[string]bool{}
	for _, h := range d.Histories {
		id := strconv.FormatInt(h.Record.ID, 10)
		v.HistoryIDs = append(v.HistoryIDs, id)
		v.Records = append(v.Records, MediaRemovalHistorySummary{HistoryRecord: h.Record, ID: id})
		for _, r := range h.Rows {
			if r.Table != "TRANSFER_HISTORY" {
				key := Digest(r)
				if !seen[key] {
					v.DedupeRows++
					seen[key] = true
				}
			}
		}
	}
	for i, u := range d.Units {
		s := MediaRemovalScopeView{Ordinal: i, RootID: u.Definition.Scope.Root.ID, Driver: recoveryDriver(u.Definition.Scope), Path: u.Definition.Path, Role: u.Definition.Removal.Role, Absent: u.Absent, Sample: []string{}, Events: u.Events}
		for _, n := range u.Definition.Nodes {
			switch n.Kind {
			case "file":
				s.Files++
				s.Bytes += n.Node.Size
			case "link":
				s.Links++
			case "directory":
				s.Directories++
			}
			if len(s.Sample) < 100 {
				s.Sample = append(s.Sample, n.Path)
			}
		}
		v.Units = append(v.Units, s)
	}
	return v
}
