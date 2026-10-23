package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

type mediaRemovalPath struct {
	RootID    string `json:"rootId"`
	Driver    string `json:"driver"`
	Path      string `json:"path"`
	HistoryID string `json:"historyId,omitempty"`
	Role      string `json:"role,omitempty"`
}
type mediaRemovalInput struct {
	HistoryIDs                 []string           `json:"historyIds"`
	Flag                       string             `json:"flag"`
	Paths                      []mediaRemovalPath `json:"paths"`
	IncludeNFO                 bool               `json:"includeNfo"`
	CleanupDirectories         bool               `json:"cleanupDirectories"`
	RequestID                  string             `json:"requestId"`
	Decision                   string             `json:"decision"`
	Fingerprint                string             `json:"fingerprint"`
	Ordinal                    int                `json:"ordinal"`
	Confirm                    bool               `json:"confirm"`
	ConfirmRead                bool               `json:"confirmRead"`
	ConfirmHistoryDeletion     bool               `json:"confirmHistoryDeletion"`
	ConfirmDedupeReset         bool               `json:"confirmDedupeReset"`
	ConfirmPermanentDeletion   bool               `json:"confirmPermanentDeletion"`
	ConfirmOnlyCopyRisk        bool               `json:"confirmOnlyCopyRisk"`
	ConfirmExpandedScope       bool               `json:"confirmExpandedScope"`
	ConfirmOldExecutorsStopped bool               `json:"confirmOldExecutorsStopped"`
	ConfirmCloseRelated        bool               `json:"confirmCloseRelated"`
	ConfirmRemoteExclusive     bool               `json:"confirmRemoteExclusive"`
	ConfirmClose               bool               `json:"confirmClose"`
	ConfirmReturnCapture       bool               `json:"confirmReturnCapture"`
}

func mediaRemovalInputDigest(in mediaRemovalInput) string {
	return organization.Digest([]any{in.HistoryIDs, in.Flag, in.Paths, in.IncludeNFO, in.CleanupDirectories, strings.TrimSpace(in.Decision)})
}
func mediaRemovalIDs(in mediaRemovalInput) ([]int64, error) {
	if len(in.HistoryIDs) > 100 || len(in.Paths) > 1024 || len(in.HistoryIDs) == 0 && len(in.Paths) == 0 {
		return nil, organization.ErrLimit
	}
	if in.Flag != "" && in.Flag != "del_source" && in.Flag != "del_dest" && in.Flag != "del_all" {
		return nil, organization.ErrState
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, raw := range in.HistoryIDs {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != raw || seen[id] {
			return nil, organization.ErrState
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}
func mediaRemovalRelative(p string) bool {
	return p != "" && p != "." && len(p) <= 4096 && !path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsAny(p, "\\\x00\r\n") && p != ".." && !strings.HasPrefix(p, "../")
}
func mediaRemovalStoredPath(dir, name string) (string, error) {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || name == "" || path.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(dir+name, "\\\x00\r\n") {
		return "", organization.ErrPath
	}
	return path.Join(dir, name), nil
}
func (api *organizationAPI) mediaRemovalLocation(ctx context.Context, absolute, role, driver string) (mediaRemovalPath, error) {
	if !path.IsAbs(absolute) || path.Clean(absolute) != absolute || absolute == "/" {
		return mediaRemovalPath{}, organization.ErrPath
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return mediaRemovalPath{}, err
	}
	options := roots.Targets
	if role == "source" {
		options = roots.Sources
	} else if role == "file" {
		options = append(append([]organizationRoot{}, roots.Sources...), roots.Targets...)
	}
	var selected mediaRemovalPath
	best := 0
	for _, root := range options {
		aliases := []string{root.Path}
		if driver == "" {
			if canonical, err := filepath.EvalSymlinks(root.Path); err == nil && canonical != root.Path {
				aliases = append(aliases, canonical)
			}
		}
		for _, base := range aliases {
			if !strings.HasPrefix(absolute, base+"/") {
				continue
			}
			relative := strings.TrimPrefix(absolute, base+"/")
			if mediaRemovalRelative(relative) && len(base) > best {
				selected = mediaRemovalPath{RootID: root.ID, Driver: driver, Path: relative, Role: role}
				best = len(base)
			}
		}
	}
	if selected.RootID == "" {
		return selected, fmt.Errorf("%w: recorded %s is outside its configured root", organization.ErrPath, role)
	}
	return selected, nil
}

// Older aggregate rows may omit DEST_PATH/DEST_FILENAME. Render only the
// directory part of the current legacy naming template from the stored media
// identity, and require the resulting complete directory in the new review.
// Unresolvable/custom historical layouts can be selected explicitly instead.
func (api *organizationAPI) mediaRemovalLegacyDirectory(ctx context.Context, h organization.HistoryRecord) (string, *mediameta.EpisodeInfo, error) {
	if strings.TrimSpace(h.Title) == "" {
		return "", nil, organization.ErrPath
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return "", nil, err
	}
	media := objectValue(snapshot["media"])
	tv := h.Type != "电影" && h.Type != "Movie" && h.Type != "MOV"
	format := localNamingTemplate(media, tv)
	i := strings.LastIndex(format, "/")
	if i < 1 {
		return "", nil, organization.ErrPath
	}
	format = format[:i]
	values := map[string]string{"title": localLibraryName(h.Title, media), "name": localLibraryName(h.Title, media), "year": h.Year, "tmdbid": h.TMDBID, "season_episode": h.SeasonEpisode}
	var episodes *mediameta.EpisodeInfo
	if tv {
		e, err := mediameta.Episodes("Show "+h.SeasonEpisode, "")
		if err != nil || e.Season == nil || e.EndSeason != nil {
			return "", nil, organization.ErrPath
		}
		values["season"] = strconv.Itoa(*e.Season)
		episodes = &e
	}
	relative, err := renderLocalDirectory(format, values)
	if err != nil || !mediaRemovalRelative(relative) {
		return "", nil, organization.ErrPath
	}
	categories, err := api.recognition.categories.load()
	if err != nil {
		return "", nil, err
	}
	categoryKind := "movie"
	if tv {
		categoryKind = "tv"
	}
	enabled, err := categoryEnabled(categories, categoryKind)
	if err != nil {
		return "", nil, err
	}
	if enabled && h.Category != "" {
		if path.Base(h.Category) != h.Category || h.Category == ".." || h.Category == "." {
			return "", nil, organization.ErrPath
		}
		relative = path.Join(h.Category, relative)
	}
	if !path.IsAbs(h.Dest) || path.Clean(h.Dest) != h.Dest {
		return "", nil, organization.ErrPath
	}
	return path.Join(h.Dest, relative), episodes, nil
}

func (api *organizationAPI) mediaRemovalSelections(ctx context.Context, in mediaRemovalInput, records []organization.HistoryRecord) ([]mediaRemovalPath, error) {
	paths := []mediaRemovalPath{}
	known := map[string]organization.HistoryRecord{}
	overrides := map[string]mediaRemovalPath{}
	roots, err := api.roots(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range records {
		known[strconv.FormatInt(h.ID, 10)] = h
	}
	for _, p := range in.Paths {
		if !mediaRemovalRelative(p.Path) || p.RootID == "" || p.Driver != "" && organization.LibraryRemoteMode(p.Driver) == "" {
			return nil, organization.ErrPath
		}
		allowed := p.Role == "" || p.Role == "file"
		if p.Role == "source" {
			for _, r := range roots.Sources {
				allowed = allowed || r.ID == p.RootID
			}
			if p.Driver != "" {
				return nil, organization.ErrMode
			}
		} else if p.Role == "library" {
			for _, r := range roots.Targets {
				allowed = allowed || r.ID == p.RootID
			}
		}
		if !allowed {
			return nil, organization.ErrPath
		}
		if p.HistoryID == "" {
			if p.Role != "" && p.Role != "file" {
				return nil, organization.ErrPath
			}
			p.Role = "file"
			paths = append(paths, p)
			continue
		}
		if _, yes := known[p.HistoryID]; !yes || p.Role != "source" && p.Role != "library" {
			return nil, organization.ErrState
		}
		key := p.HistoryID + ":" + p.Role
		if _, yes := overrides[key]; yes {
			return nil, organization.ErrState
		}
		overrides[key] = p
	}
	used := map[string]bool{}
	for _, h := range records {
		id := strconv.FormatInt(h.ID, 10)
		for _, role := range []string{"source", "library"} {
			if in.Flag == "" || role == "source" && in.Flag == "del_dest" || role == "library" && in.Flag == "del_source" {
				continue
			}
			if p, yes := overrides[id+":"+role]; yes {
				paths = append(paths, p)
				used[id+":"+role] = true
				continue
			}
			driver := ""
			if role == "library" {
				mode := transferModes[h.Mode]
				if h.Mode != "" && mode == "" {
					return nil, organization.ErrMode
				}
				if h.Mode == "" {
					var err error
					mode, err = api.legacyRequestMode("")
					if err != nil {
						return nil, err
					}
				}
				if organization.RemoteMode(mode) {
					driver = "rclone"
					if strings.HasPrefix(mode, "minio") {
						driver = "minio"
					}
				}
			}
			var absolute string
			var err error
			if role == "source" {
				absolute, err = mediaRemovalStoredPath(h.SourcePath, h.SourceFilename)
			} else if h.DestPath != "" && h.DestFilename != "" {
				absolute, err = mediaRemovalStoredPath(h.DestPath, h.DestFilename)
			} else {
				var episodes *mediameta.EpisodeInfo
				absolute, episodes, err = api.mediaRemovalLegacyDirectory(ctx, h)
				if err == nil && episodes != nil && episodes.Episode != nil {
					p, err := api.mediaRemovalLocation(ctx, absolute, role, driver)
					if err != nil {
						return nil, err
					}
					scope, c, err := api.recoveryScope(ctx, p.RootID, p.Driver)
					if err != nil {
						return nil, err
					}
					entries, err := organization.RecoveryBrowse(ctx, scope, p.Path, c)
					if c != nil {
						c.Close()
					}
					if err != nil {
						return nil, err
					}
					last := *episodes.Episode
					if episodes.EndEpisode != nil {
						last = *episodes.EndEpisode
					}
					before := len(paths)
					for _, entry := range entries {
						if entry.Directory || organization.FileKind(entry.Name) != "media" {
							continue
						}
						e, err := mediameta.Episodes(entry.Name, "")
						if err != nil || e.Season == nil || *e.Season != *episodes.Season || e.Episode == nil || e.EndSeason != nil {
							continue
						}
						end := *e.Episode
						if e.EndEpisode != nil {
							end = *e.EndEpisode
						}
						if *e.Episode >= *episodes.Episode && end <= last {
							copy := p
							copy.Path = entry.Path
							copy.HistoryID = id
							paths = append(paths, copy)
						}
					}
					if len(paths) == before {
						return nil, fmt.Errorf("%w: no unambiguous episode file; select the intended full configured path explicitly", organization.ErrPath)
					}
					continue
				}
			}
			if err != nil {
				return nil, fmt.Errorf("%w: history #%s needs an explicit configured %s selection", organization.ErrPath, id, role)
			}
			p, err := api.mediaRemovalLocation(ctx, absolute, role, driver)
			if err != nil {
				return nil, err
			}
			p.HistoryID = id
			paths = append(paths, p)
		}
	}
	if len(used) != len(overrides) {
		return nil, organization.ErrState
	}
	return paths, nil
}

type mediaRemovalReadUnit struct {
	unit   organization.MediaRemovalUnit
	client *remotestore.Client
}

func mediaRemovalMergeUnits(units []mediaRemovalReadUnit) []mediaRemovalReadUnit {
	sort.SliceStable(units, func(i, j int) bool {
		a, b := units[i].unit.Definition, units[j].unit.Definition
		return path.Join(a.Scope.Root.Path, a.Path) < path.Join(b.Scope.Root.Path, b.Path)
	})
	out := []mediaRemovalReadUnit{}
	for _, u := range units {
		merged := false
		for i := range out {
			a, b := out[i].unit.Definition, u.unit.Definition
			if organization.Digest(a.Scope.Remote) != organization.Digest(b.Scope.Remote) || a.Scope.ConfigDigest != b.Scope.ConfigDigest {
				continue
			}
			ap, bp := path.Join(a.Scope.Root.Path, a.Path), path.Join(b.Scope.Root.Path, b.Path)
			if bp == ap || strings.HasPrefix(bp, ap+"/") {
				for _, ref := range u.unit.Events {
					ref.Path = strings.TrimPrefix(path.Join(b.Scope.Root.Path, ref.Path), a.Scope.Root.Path+"/")
					out[i].unit.Events = append(out[i].unit.Events, ref)
				}
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, u)
		}
	}
	for i := range out {
		seen := map[string]bool{}
		refs := []organization.MediaRemovalEventRef{}
		for _, r := range out[i].unit.Events {
			key := organization.Digest(r)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, r)
			}
		}
		out[i].unit.Events = refs
	}
	return out
}

func (api *organizationAPI) prepareMediaRemoval(ctx context.Context, in mediaRemovalInput, actor string) (organization.MediaRemovalReview, func(), error) {
	empty := func() {}
	ids, err := mediaRemovalIDs(in)
	if err != nil {
		return organization.MediaRemovalReview{}, empty, err
	}
	if !in.ConfirmRead || strings.TrimSpace(in.Decision) == "" || len(in.Decision) > 512 {
		return organization.MediaRemovalReview{}, empty, organization.ErrState
	}
	if in.RequestID == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return organization.MediaRemovalReview{}, empty, err
		}
		in.RequestID = hex.EncodeToString(nonce[:])
	}
	nonce, err := hex.DecodeString(in.RequestID)
	if err != nil || len(nonce) != 16 || hex.EncodeToString(nonce) != in.RequestID {
		return organization.MediaRemovalReview{}, empty, organization.ErrState
	}
	records, _, err := api.jobs.MediaRemovalHistories(ctx, ids)
	if err != nil {
		return organization.MediaRemovalReview{}, empty, err
	}
	paths, err := api.mediaRemovalSelections(ctx, in, records)
	if err != nil {
		return organization.MediaRemovalReview{}, empty, err
	}
	d := organization.MediaRemovalDefinition{Actor: actor, RequestID: in.RequestID, Decision: strings.TrimSpace(in.Decision), InputDigest: mediaRemovalInputDigest(in), Units: []organization.MediaRemovalUnit{}}
	opened := map[string]*remotestore.Client{}
	scopes := map[string]organization.RecoveryScope{}
	closeClients := func() {
		for _, c := range opened {
			if c != nil {
				c.Close()
			}
		}
	}
	units := []mediaRemovalReadUnit{}
	for _, p := range paths {
		key := p.RootID + ":" + p.Driver
		s, yes := scopes[key]
		if !yes {
			var c *remotestore.Client
			s, c, err = api.recoveryScope(ctx, p.RootID, p.Driver)
			if err != nil {
				closeClients()
				return organization.MediaRemovalReview{}, empty, err
			}
			scopes[key], opened[key] = s, c
		}
		u := organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Scope: s, Path: p.Path, Removal: &organization.MediaRemovalOrigin{Parent: organization.MediaRemovalID(actor, in.RequestID), Role: p.Role}}, Events: []organization.MediaRemovalEventRef{}}
		if p.HistoryID != "" {
			u.Events = append(u.Events, organization.MediaRemovalEventRef{HistoryID: p.HistoryID, Role: p.Role, Path: p.Path})
		}
		units = append(units, mediaRemovalReadUnit{u, opened[key]})
		d.Units = append(d.Units, u)
	}
	owners, err := api.jobs.MediaRemovalGuardOwners(ctx, d, ids)
	if err != nil {
		closeClients()
		return organization.MediaRemovalReview{}, empty, err
	}
	closeGuards, err := api.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: owners})
	if err != nil {
		closeClients()
		return organization.MediaRemovalReview{}, empty, err
	}
	closeAll := func() { closeGuards(); closeClients() }
	call, stop := recoveryWatch(ctx, func(call context.Context) error {
		for _, s := range scopes {
			if err := api.recoveryScopeCurrent(call, s); err != nil {
				return err
			}
		}
		return nil
	})
	defer stop()
	read := func(u *mediaRemovalReadUnit) error {
		present, err := organization.MediaRemovalPresence(call, u.unit.Definition.Scope, u.unit.Definition.Path, u.client)
		if err != nil {
			return err
		}
		u.unit.Absent = !present
		if present {
			u.unit.Definition.Nodes, err = organization.MediaRemovalSnapshot(call, u.unit.Definition.Scope, u.unit.Definition.Path, u.client)
			refs := []organization.MediaRemovalEventRef{}
			for _, ref := range u.unit.Events {
				for _, node := range u.unit.Definition.Nodes {
					if path.Join(u.unit.Definition.Path, node.Path) == ref.Path {
						refs = append(refs, ref)
						break
					}
				}
			}
			u.unit.Events = refs
		} else {
			u.unit.Definition.Nodes = []organization.RecoveryNode{}
			u.unit.Events = []organization.MediaRemovalEventRef{}
		}
		return err
	}
	units = mediaRemovalMergeUnits(units)
	for i := range units {
		if err := read(&units[i]); err != nil {
			closeAll()
			return organization.MediaRemovalReview{}, empty, err
		}
		if err := mediaRemovalReadBudget(units); err != nil {
			closeAll()
			return organization.MediaRemovalReview{}, empty, err
		}
	}
	if in.IncludeNFO {
		initial := append([]mediaRemovalReadUnit{}, units...)
		for _, u := range initial {
			if u.unit.Absent || len(u.unit.Definition.Nodes) == 0 || u.unit.Definition.Nodes[0].Kind == "directory" {
				continue
			}
			p := u.unit.Definition.Path
			ext := path.Ext(p)
			if ext == "" || strings.EqualFold(ext, ".nfo") {
				continue
			}
			copy := u
			copy.unit.Events = []organization.MediaRemovalEventRef{}
			copy.unit.Definition = organization.RecoveryDefinition{Scope: u.unit.Definition.Scope, Path: strings.TrimSuffix(p, ext) + ".nfo", Removal: &organization.MediaRemovalOrigin{Parent: organization.MediaRemovalID(actor, in.RequestID), Role: "cleanup"}}
			if err := read(&copy); err != nil {
				closeAll()
				return organization.MediaRemovalReview{}, empty, err
			}
			if !copy.unit.Absent {
				units = mediaRemovalMergeUnits(append(units, copy))
				if err := mediaRemovalReadBudget(units); err != nil {
					closeAll()
					return organization.MediaRemovalReview{}, empty, err
				}
			}
		}
	}
	if in.CleanupDirectories {
		units, err = api.mediaRemovalCleanup(call, units, read)
		if err != nil {
			closeAll()
			return organization.MediaRemovalReview{}, empty, err
		}
	}
	units = mediaRemovalMergeUnits(units)
	d.Units = []organization.MediaRemovalUnit{}
	for _, u := range units {
		d.Units = append(d.Units, u.unit)
	}
	v, err := api.jobs.PrepareMediaRemoval(ctx, d, ids)
	if err != nil {
		closeAll()
		return v, empty, err
	}
	// Cleanup may widen a parent scope. Discover the complete resulting owner
	// set and require those writers' guards before accepting the final snapshot.
	if organization.Digest(v.Owners) != organization.Digest(owners) {
		closeGuards()
		closeGuards, err = api.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: v.Owners})
		if err != nil {
			closeClients()
			return v, empty, err
		}
		expected := organization.Digest(v.Owners)
		for i := range units {
			if err := read(&units[i]); err != nil {
				closeAll()
				return v, empty, err
			}
			if err := mediaRemovalReadBudget(units); err != nil {
				closeAll()
				return v, empty, err
			}
		}
		d.Units = []organization.MediaRemovalUnit{}
		for _, u := range units {
			d.Units = append(d.Units, u.unit)
		}
		v, err = api.jobs.PrepareMediaRemoval(ctx, d, ids)
		if err != nil {
			closeAll()
			return v, empty, err
		}
		if organization.Digest(v.Owners) != expected {
			closeAll()
			return v, empty, organization.ErrBusy
		}
	}
	for _, s := range scopes {
		if err := api.recoveryScopeCurrent(ctx, s); err != nil {
			closeAll()
			return v, empty, err
		}
	}
	return v, closeAll, nil
}

func (api *organizationAPI) mediaRemovalCleanup(ctx context.Context, units []mediaRemovalReadUnit, read func(*mediaRemovalReadUnit) error) ([]mediaRemovalReadUnit, error) {
	initial := append([]mediaRemovalReadUnit{}, units...)
	seen := map[string]bool{}
	for _, u := range initial {
		if u.unit.Absent || len(u.unit.Definition.Nodes) == 0 || u.unit.Definition.Nodes[0].Kind == "directory" {
			continue
		}
		parent := path.Dir(u.unit.Definition.Path)
		candidates := []string{parent}
		name := strings.ToLower(path.Base(parent))
		season := strings.HasPrefix(name, "season") || len(name) > 1 && name[0] == 's' && name[1] >= '0' && name[1] <= '9'
		if season {
			candidates = append(candidates, path.Dir(parent))
		}
		for _, p := range candidates {
			if !mediaRemovalRelative(p) {
				continue
			}
			key := organization.Digest(u.unit.Definition.Scope) + ":" + p
			if seen[key] {
				continue
			}
			seen[key] = true
			candidate := mediaRemovalReadUnit{client: u.client, unit: organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Scope: u.unit.Definition.Scope, Path: p, Removal: &organization.MediaRemovalOrigin{Parent: u.unit.Definition.Removal.Parent, Role: "cleanup"}}, Events: []organization.MediaRemovalEventRef{}}}
			if err := read(&candidate); err != nil {
				return nil, err
			}
			if candidate.unit.Absent {
				continue
			}
			remainingMedia := false
			for _, node := range candidate.unit.Definition.Nodes {
				if node.Kind == "directory" || organization.FileKind(node.Path) != "media" {
					continue
				}
				full := path.Join(p, node.Path)
				covered := false
				for _, selected := range units {
					if organization.Digest(selected.unit.Definition.Scope) != organization.Digest(candidate.unit.Definition.Scope) {
						continue
					}
					q := selected.unit.Definition.Path
					if full == q || strings.HasPrefix(full, q+"/") {
						covered = true
						break
					}
				}
				if !covered {
					remainingMedia = true
					break
				}
			}
			if !remainingMedia {
				units = mediaRemovalMergeUnits(append(units, candidate))
				if err := mediaRemovalReadBudget(units); err != nil {
					return nil, err
				}
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return units, nil
}

func mediaRemovalReadBudget(units []mediaRemovalReadUnit) error {
	objects := 0
	var bytes int64
	for _, u := range units {
		if len(u.unit.Definition.Nodes) > organization.RecoveryObjectLimit-objects {
			return organization.ErrLimit
		}
		objects += len(u.unit.Definition.Nodes)
		for _, node := range u.unit.Definition.Nodes {
			if node.Kind == "file" {
				if node.Node.Size < 0 || node.Node.Size > organization.RecoveryByteLimit-bytes {
					return organization.ErrLimit
				}
				bytes += node.Node.Size
			}
		}
	}
	return nil
}
