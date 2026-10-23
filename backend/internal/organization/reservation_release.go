package organization

// Releasing a reservation is a ledger-only negative disposition. It proves
// neither that a file was published nor that it is absent. Old writers are
// permanently closed before their exact claims can be released. A later writer
// must use a new job, a fresh content review and the ordinary publication rules.
import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

const ReservationReleaseOwnerLimit = 10000
const ReservationReleaseRowLimit = 50000
const reservationReleasePathLimit = 200000

type ReservationOwnerRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type ReservationClaim struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Path string `json:"path"`
}
type ReservationOwner struct {
	ReservationOwnerRef
	State     string             `json:"state"`
	BatchID   string             `json:"batchId,omitempty"`
	Related   bool               `json:"related"`
	PathCount int                `json:"pathCount"`
	Paths     []string           `json:"paths"`
	Claims    []ReservationClaim `json:"claims"`
}
type ReservationRelease struct {
	ID          string                     `json:"id"`
	State       string                     `json:"state"`
	Seed        ReservationOwnerRef        `json:"seed"`
	Actor       string                     `json:"actor"`
	RequestID   string                     `json:"requestId"`
	Decision    string                     `json:"decision"`
	Fingerprint string                     `json:"fingerprint"`
	Created     string                     `json:"created"`
	Owners      []ReservationOwner         `json:"owners"`
	ClaimCount  int                        `json:"claimCount"`
	RowCount    int                        `json:"rowCount"`
	Document    reservationReleaseDocument `json:"-"`
}
type reservationReleaseDocument struct {
	Seed      ReservationOwnerRef `json:"seed"`
	Actor     string              `json:"actor"`
	RequestID string              `json:"requestId"`
	Decision  string              `json:"decision"`
	Owners    []ReservationOwner  `json:"owners"`
	Rows      []MediaRemovalRow   `json:"rows"`
}
type reservationRecord struct {
	ref          ReservationOwnerRef
	state, batch string
	paths        []string
	relations    []string
	groups       []string
	aliases      []string
}

// Model a stored same-identity root alias without probing an offline mount.
// Virtual keys occupy a different namespace from absolute user paths.
func reservationRootAliases(ctx context.Context, paths []string, roots []LibraryRoot) ([]string, error) {
	result := []string{}
	if len(roots) > 2048 || len(paths) > reservationReleasePathLimit {
		return nil, ErrLimit
	}
	seenRoots, seenPaths := map[string]bool{}, map[string]bool{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if root.Identity == "" || !path.IsAbs(root.Path) || path.Clean(root.Path) == "/" {
			continue
		}
		base := path.Clean(root.Path)
		binding := Digest([]string{root.Identity, base})
		if seenRoots[binding] {
			continue
		}
		seenRoots[binding] = true
		for index, p := range paths {
			if index%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			p = path.Clean(p)
			if p != base && !strings.HasPrefix(p, base+"/") {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
			key := "frozen-root:" + Digest(root.Identity)
			if rel != "" {
				key += "/" + rel
			}
			if seenPaths[key] {
				continue
			}
			seenPaths[key] = true
			result = append(result, key)
			if len(result) > reservationReleasePathLimit {
				return nil, ErrLimit
			}
		}
	}
	return result, nil
}

func reservationKey(ref ReservationOwnerRef) string { return ref.Kind + ":" + ref.ID }
func ReservationReleaseID(actor, request string) string {
	return Digest([]string{"independent-reservation-release", actor, request})
}
func validReservationRef(ref ReservationOwnerRef) bool {
	switch ref.Kind {
	case "organization":
		return recoveryHex(ref.ID, 32)
	case "library", "recovery", "media-removal":
		return recoveryHex(ref.ID, 64)
	}
	return false
}

func reservationRecords(ctx context.Context, r recoveryReader) ([]reservationRecord, error) {
	base, err := recoveryOwnerRecords(ctx, r)
	if err != nil {
		return nil, err
	}
	records := []reservationRecord{}
	for _, rec := range base {
		v := reservationRecord{ref: ReservationOwnerRef{rec.kind, rec.id}, state: rec.state, paths: append(append([]string{}, rec.protected...), rec.paths...)}
		if rec.kind == "organization" {
			v.batch = rec.batch
			var d Definition
			if json.Unmarshal([]byte(rec.definition), &d) != nil {
				return nil, ErrState
			}
			if d.History != nil {
				v.groups = append(v.groups, "history:"+strconv.FormatInt(d.History.ID, 10))
			}
			if d.Unknown != nil {
				v.groups = append(v.groups, "unknown:"+strconv.FormatInt(d.Unknown.ID, 10))
			}
			v.aliases, err = reservationRootAliases(ctx, v.paths, []LibraryRoot{{Path: d.SourceRoot, Identity: d.SourceIdentity}, {Path: d.TargetRoot, Identity: d.TargetIdentity}})
			if err != nil {
				return nil, err
			}
		} else {
			var d LibraryDefinition
			if json.Unmarshal([]byte(rec.definition), &d) != nil {
				return nil, ErrState
			}
			if d.Schedule != nil {
				v.batch = d.Schedule.Batch
			}
			v.aliases, err = reservationRootAliases(ctx, v.paths, append([]LibraryRoot{d.Root}, d.Allowed...))
			if err != nil {
				return nil, err
			}
		}
		records = append(records, v)
	}
	for _, kind := range []string{"recovery", "media-removal"} {
		table := "GO_RECOVERY_DISPOSALS"
		if kind == "media-removal" {
			table = "GO_MEDIA_REMOVALS"
		}
		yes, err := recoveryTable(ctx, r, table)
		if err != nil {
			return nil, err
		}
		if !yes {
			continue
		}
		rows, err := recoveryRows(ctx, r, `SELECT ID,STATE,DEFINITION FROM `+table+` ORDER BY ID`)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			v := reservationRecord{ref: ReservationOwnerRef{kind, row[0]}, state: row[1], paths: []string{}, relations: []string{}}
			add := func(d RecoveryDefinition, id string) error {
				if !recoveryScopeValid(d.Scope) || !recoveryDefinitionPath(d) {
					return ErrState
				}
				unitPaths := []string{path.Join(d.Scope.Root.Path, d.Path)}
				if d.Scope.Remote == nil && id != "" {
					unitPaths = append(unitPaths, path.Join(d.Scope.Root.Path, recoveryCapture(id, d.Path)))
				}
				v.paths = append(v.paths, unitPaths...)
				aliases, err := reservationRootAliases(ctx, unitPaths, []LibraryRoot{d.Scope.Root})
				if err != nil {
					return err
				}
				v.aliases = append(v.aliases, aliases...)
				for _, o := range d.Owners {
					v.relations = append(v.relations, reservationKey(ReservationOwnerRef{o.Kind, o.ID}))
				}
				return nil
			}
			if kind == "recovery" {
				var d RecoveryDefinition
				if json.Unmarshal([]byte(row[2]), &d) != nil || RecoveryDisposalID(d.Actor, d.RequestID) != row[0] {
					return nil, ErrState
				}
				if err := add(d, row[0]); err != nil {
					return nil, err
				}
				if d.Removal != nil {
					v.relations = append(v.relations, "media-removal:"+d.Removal.Parent)
				}
			} else {
				var d MediaRemovalDefinition
				if json.Unmarshal([]byte(row[2]), &d) != nil || MediaRemovalID(d.Actor, d.RequestID) != row[0] {
					return nil, ErrState
				}
				for i, u := range d.Units {
					if err := add(u.Definition, mediaRemovalChildID(d, i)); err != nil {
						return nil, err
					}
				}
				for _, o := range d.Owners {
					v.relations = append(v.relations, reservationKey(ReservationOwnerRef{o.Kind, o.ID}))
				}
				for _, h := range d.Histories {
					v.groups = append(v.groups, "history:"+strconv.FormatInt(h.Record.ID, 10))
				}
			}
			records = append(records, v)
			if len(records) > ReservationReleaseOwnerLimit {
				return nil, ErrLimit
			}
		}
	}
	if len(records) > ReservationReleaseOwnerLimit {
		return nil, ErrLimit
	}
	for _, rec := range records {
		if !validReservationRef(rec.ref) {
			return nil, ErrState
		}
	}
	sort.Slice(records, func(i, j int) bool { return reservationKey(records[i].ref) < reservationKey(records[j].ref) })
	return records, nil
}

// Build complete connected components, not a successful subset. Sorted prefix
// intervals avoid a quadratic all-jobs/all-paths scan. Old batch siblings and
// parent/child disposal references are edges even when their roots differ.
func reservationClosure(ctx context.Context, r recoveryReader, seed ReservationOwnerRef) ([]reservationRecord, error) {
	if !validReservationRef(seed) {
		return nil, fmt.Errorf("%w: invalid reservation seed %q", ErrPath, reservationKey(seed))
	}
	records, err := reservationRecords(ctx, r)
	if err != nil {
		return nil, err
	}
	indices := map[string]int{}
	parent := make([]int, len(records))
	released := make([]bool, len(records))
	for i, rec := range records {
		indices[reservationKey(rec.ref)] = i
		parent[i] = i
		released[i], err = reservationReleased(ctx, r, rec.ref.Kind, rec.ref.ID)
		if err != nil {
			return nil, err
		}
	}
	start, yes := indices[reservationKey(seed)]
	if !yes {
		return nil, ErrNotFound
	}
	if released[start] {
		return nil, ErrState
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		a, b = find(a), find(b)
		if a != b {
			parent[a] = b
		}
	}
	type location struct {
		key, order string
		owner      int
	}
	locations := []location{}
	groups := map[string]int{}
	for i, rec := range records {
		if released[i] {
			continue
		}
		if rec.batch != "" {
			key := rec.ref.Kind + ":" + rec.batch
			if j, ok := groups[key]; ok {
				union(i, j)
			} else {
				groups[key] = i
			}
		}
		for _, key := range rec.groups {
			if j, ok := groups[key]; ok {
				union(i, j)
			} else {
				groups[key] = i
			}
		}
		for _, key := range rec.relations {
			j, ok := indices[key]
			if !ok {
				return nil, ErrState
			}
			if !released[j] {
				union(i, j)
			}
		}
		for _, p := range rec.paths {
			if !path.IsAbs(p) || len(p) > 4096 || path.Clean(p) == "/" || strings.ContainsAny(p, "\\\x00\r\n") {
				return nil, fmt.Errorf("%w: invalid stored reservation path %q for %s", ErrPath, p, reservationKey(rec.ref))
			}
			key := recoveryPathKey(p)
			locations = append(locations, location{key, strings.ReplaceAll(key, "/", "\x00"), i})
			if len(locations) > reservationReleasePathLimit {
				return nil, ErrLimit
			}
		}
		for _, alias := range rec.aliases {
			key := recoveryPathKey(alias)
			locations = append(locations, location{key, strings.ReplaceAll(key, "/", "\x00"), i})
			if len(locations) > reservationReleasePathLimit {
				return nil, ErrLimit
			}
		}
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].order < locations[j].order })
	stack := []location{}
	for _, p := range locations {
		for len(stack) > 0 && !recoveryOverlaps(stack[len(stack)-1].key, p.key) {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			union(p.owner, stack[len(stack)-1].owner)
		}
		stack = append(stack, p)
	}
	result := []reservationRecord{}
	for i, rec := range records {
		if !released[i] && find(i) == find(start) {
			result = append(result, rec)
		}
	}
	return result, nil
}

func reservationProjection(d reservationReleaseDocument) ReservationRelease {
	v := ReservationRelease{ID: ReservationReleaseID(d.Actor, d.RequestID), State: "preview", Seed: d.Seed, Actor: d.Actor, RequestID: d.RequestID, Decision: d.Decision, Fingerprint: Digest(d), Owners: d.Owners, RowCount: len(d.Rows), Document: d}
	for _, o := range d.Owners {
		v.ClaimCount += len(o.Claims)
	}
	return v
}
