package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type TransferProtocol struct {
	Version     string `json:"version"`
	API         string `json:"api"`
	Engine      string `json:"engine"`
	V2          bool   `json:"v2"`
	FileIndexes bool   `json:"fileIndexes"`
	Major       int    `json:"-"`
	Minor       int    `json:"-"`
	Patch       int    `json:"-"`
}

func transferVersion(raw string, maxParts int) ([]int, bool) {
	if raw == "" || len(raw) > 64 {
		return nil, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 3 || len(parts) > maxParts {
		return nil, false
	}
	numbers := []int{}
	for _, part := range parts {
		if part == "" || len(part) > 5 {
			return nil, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return nil, false
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		numbers = append(numbers, n)
	}
	return numbers, true
}
func (c *Client) transferProtocol(ctx context.Context) (TransferProtocol, error) {
	p := TransferProtocol{}
	raw, err := c.request(ctx, http.MethodGet, "app/version", nil)
	if err != nil {
		return p, err
	}
	p.Version = strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	n, ok := transferVersion(p.Version, 3)
	if !ok || n[0] != 5 && (n[0] != 4 || n[1] < 1) {
		return p, torrenttransfer.ErrProtocol
	}
	p.Major, p.Minor, p.Patch = n[0], n[1], n[2]
	raw, err = c.request(ctx, http.MethodGet, "app/webapiVersion", nil)
	if err != nil {
		return p, err
	}
	p.API = strings.TrimSpace(string(raw))
	api, ok := transferVersion(p.API, 3)
	if !ok || api[0] != 2 {
		return p, torrenttransfer.ErrProtocol
	}
	p.FileIndexes = api[1] > 8 || api[1] == 8 && api[2] >= 2
	// Earlier qB versions cannot load v2 metainfo, irrespective of optional
	// build-info availability. No guessed engine fact is recorded for them.
	if p.Major == 4 && p.Minor < 4 {
		return p, nil
	}
	raw, err = c.request(ctx, http.MethodGet, "app/buildInfo", nil)
	if err != nil {
		return p, err
	}
	var build struct {
		Engine string `json:"libtorrent"`
	}
	if json.Unmarshal(raw, &build) != nil {
		return p, ErrResponse
	}
	n, ok = transferVersion(build.Engine, 4)
	if !ok || n[0] != 1 && n[0] != 2 {
		return p, torrenttransfer.ErrProtocol
	}
	p.Engine = build.Engine
	p.V2 = n[0] == 2 && (p.Major == 5 || p.Minor >= 4)
	return p, nil
}
func (c *Client) TransferCapabilities(ctx context.Context) (TransferProtocol, error) {
	if err := c.login(ctx); err != nil {
		return TransferProtocol{}, err
	}
	return c.transferProtocol(ctx)
}

// Libtorrent 2 get_best() uses truncated v2 for hybrid as well as pure v2.
// Only a subsequent full property-hash read can establish task ownership.
func (p TransferProtocol) WireID(m torrentmeta.TransferMetadata) (string, error) {
	if p.V2 && torrenttransfer.Hex(m.V2Hash, 64) {
		return m.V2Hash[:40], nil
	}
	if torrenttransfer.Hex(m.V1Hash, 40) {
		return m.V1Hash, nil
	}
	return "", torrenttransfer.ErrProtocol
}

type transferInfo struct {
	Tracker   string   `json:"tracker"`
	Hash      string   `json:"hash"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Added     *int64   `json:"added_on"`
	Directory *string  `json:"save_path"`
	Tags      *string  `json:"tags"`
	Auto      *bool    `json:"auto_tmm"`
	Progress  *float64 `json:"progress"`
	Left      *int64   `json:"amount_left"`
}

func (c *Client) transferInfo(ctx context.Context, id string) (transferInfo, error) {
	row := transferInfo{}
	if !torrenttransfer.Hex(id, 40) {
		return row, ErrConfiguration
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"hashes": {id}, "filter": {"all"}}.Encode(), nil)
	if err != nil {
		return row, err
	}
	var rows []transferInfo
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 1 {
		return row, ErrResponse
	}
	if len(rows) == 0 {
		return row, torrenttransfer.ErrTaskAbsent
	}
	row = rows[0]
	if !strings.EqualFold(row.Hash, id) {
		return transferInfo{}, ErrResponse
	}
	return row, nil
}

// TransferTask reads all states, including in-progress verification. Two info
// reads bracket properties/files and reject a changed task instead of joining
// file proof from one incarnation to state from another.
func (c *Client) TransferTask(ctx context.Context, id string) (torrenttransfer.Task, error) {
	s := torrenttransfer.Task{Kind: "qbittorrent", Labels: []string{}, Done: []int{}}
	if !torrenttransfer.Hex(id, 40) {
		return s, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return s, err
	}
	p, err := c.transferProtocol(ctx)
	if err != nil {
		return s, err
	}
	row, err := c.transferInfo(ctx, id)
	if err != nil {
		return s, err
	}
	if row.Added == nil || *row.Added <= 0 || row.Directory == nil || row.Auto == nil || row.Progress == nil || !validCompletionProgress(*row.Progress) || row.Left == nil || *row.Left < 0 || row.Tags == nil && (p.Major == 5 || p.Minor >= 2) {
		return s, ErrResponse
	}
	s.WireID, s.Name, s.State, s.AddedAt, s.Directory, s.AutoManaged = id, row.Name, row.State, *row.Added, *row.Directory, *row.Auto
	s.HasTracker = row.Tracker != ""
	if row.Tags != nil {
		s.RawQbTags = *row.Tags
		for _, tag := range strings.Split(*row.Tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				s.Labels = append(s.Labels, tag)
			}
		}
	}
	labels, err := torrenttransfer.Labels(s.Labels)
	if err != nil {
		return s, ErrResponse
	}
	s.Labels = labels
	switch row.State {
	case "pausedUP", "stoppedUP":
		s.Paused, s.Complete = true, true
	case "pausedDL", "stoppedDL":
		s.Paused = true
	case "uploading", "stalledUP", "queuedUP", "forcedUP":
		s.Complete, s.SourceSeeding = true, true
	case "checkingUP", "checkingDL", "checkingResumeData":
		s.Checking = true
	case "error", "missingFiles":
		s.Error = true
	case "downloading", "stalledDL", "queuedDL", "forcedDL", "metaDL", "forcedMetaDL", "moving":
	default:
		return s, ErrResponse
	}
	s.Complete = s.Complete && *row.Progress == 1 && *row.Left == 0
	if p.Major == 5 || p.Minor >= 4 {
		raw, err := c.request(ctx, http.MethodGet, "torrents/properties?"+url.Values{"hash": {id}}.Encode(), nil)
		if err != nil {
			return s, err
		}
		var props struct {
			V1 *string `json:"infohash_v1"`
			V2 *string `json:"infohash_v2"`
		}
		if json.Unmarshal(raw, &props) != nil || props.V1 == nil || props.V2 == nil {
			return s, ErrResponse
		}
		s.V1Hash, s.V2Hash = strings.ToLower(*props.V1), strings.ToLower(*props.V2)
		if s.V2Hash != "" && !p.V2 {
			return s, ErrResponse
		}
	} else {
		s.V1Hash = id
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/files?"+url.Values{"hash": {id}}.Encode(), nil)
	if err != nil {
		return s, err
	}
	var files []struct {
		Index    *int     `json:"index"`
		Name     string   `json:"name"`
		Size     *int64   `json:"size"`
		Priority *int     `json:"priority"`
		Progress *float64 `json:"progress"`
	}
	if json.Unmarshal(raw, &files) != nil || len(files) == 0 || len(files) > torrentfiles.MaxFiles {
		return s, ErrResponse
	}
	for i, f := range files {
		if f.Index == nil && !p.FileIndexes {
			f.Index = &i
		}
		if f.Index == nil || f.Size == nil || f.Priority == nil || f.Progress == nil || !validCompletionProgress(*f.Progress) || *f.Priority != 0 && *f.Priority != 1 && *f.Priority != 6 && *f.Priority != 7 {
			return s, ErrResponse
		}
		s.Files = append(s.Files, torrentfiles.File{Index: *f.Index, Path: f.Name, Size: *f.Size, Selected: *f.Priority != 0})
		if *f.Progress == 1 {
			s.Done = append(s.Done, *f.Index)
		}
	}
	after, err := c.transferInfo(ctx, id)
	if err != nil || !reflect.DeepEqual(row, after) || !s.Valid() {
		return s, ErrResponse
	}
	return s, nil
}

func (c *Client) AddTransferPaused(ctx context.Context, content []byte, directory string) (torrenttransfer.AddReceipt, error) {
	return c.addTransferPaused(ctx, content, directory, nil)
}

func (c *Client) AddReviewedTransfer(ctx context.Context, content []byte, directory string, intent torrenttransfer.AddIntent) (torrenttransfer.AddReceipt, error) {
	if err := intent.Validate(); err != nil {
		return torrenttransfer.AddReceipt{}, err
	}
	return c.addTransferPaused(ctx, content, directory, &intent)
}

func (c *Client) addTransferPaused(ctx context.Context, content []byte, directory string, intent *torrenttransfer.AddIntent) (torrenttransfer.AddReceipt, error) {
	r := torrenttransfer.AddReceipt{}
	m, err := torrentmeta.PrepareTransfer(content, nil)
	if err != nil {
		return r, ErrConfiguration
	}
	directory, err = torrenttransfer.NormalizeRemote(directory)
	if err != nil || strings.TrimSpace(directory) != directory {
		return r, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return r, err
	}
	p, err := c.transferProtocol(ctx)
	if err != nil {
		return r, err
	}
	r.WireID, err = p.WireID(m)
	if err != nil {
		return r, err
	}
	if _, err := c.transferInfo(ctx, r.WireID); err == nil {
		r.Duplicate = true
		return r, nil
	} else if err != torrenttransfer.ErrTaskAbsent {
		return r, err
	}
	form := url.Values{"savepath": {directory}, "autoTMM": {"false"}, "skip_checking": {"false"}, "tags": {"已整理,转移做种"}}
	if p.Major == 5 {
		form.Set("stopped", "true")
	} else {
		form.Set("paused", "true")
	}
	if p.Major == 5 || p.Minor > 3 || p.Minor == 3 && p.Patch >= 2 {
		form.Set("contentLayout", "Original")
	} else {
		// The old switch means create a subfolder, including for a single
		// payload. Preserve the metadata's actual top-level layout.
		form.Set("root_folder", strconv.FormatBool(strings.Contains(m.Files[0].Path, "/")))
	}
	if m.Private {
		if p.Major == 4 && p.Minor < 6 {
			return r, torrenttransfer.ErrProtocol
		}
		form.Set("stopCondition", "None")
		form.Set("ratioLimit", "-1")
		form.Set("seedingTimeLimit", "-1")
		form.Set("inactiveSeedingTimeLimit", "-1")
	}
	send := func() (torrenttransfer.AddReceipt, error) { return r, c.addTorrentForm(ctx, m.Content, form, r.WireID) }
	if intent != nil {
		return intent.Send(ctx, c.Address(), r.WireID, directory, m.Content, send)
	}
	return send()
}

func (c *Client) transferAction(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata, start bool) error {
	if expected.Kind != "qbittorrent" || !expected.Matches(m) || !expected.Paused || expected.Checking || expected.Error || expected.AutoManaged || start && !expected.FullComplete(m) {
		return ErrConfiguration
	}
	current, err := c.TransferTask(ctx, expected.WireID)
	if err != nil {
		return err
	}
	if !expected.SameInstance(current) || !current.Paused || current.Checking || current.Error || start && (!current.HasTracker || !current.FullComplete(m)) {
		return ErrResponse
	}
	if start && m.Private {
		profile, err := c.Profile(ctx, expected.WireID)
		if err != nil {
			return err
		}
		dir, _ := torrenttransfer.NormalizeRemote(profile.Directory)
		want, _ := torrenttransfer.NormalizeRemote(current.Directory)
		if !profile.Paused() || !profile.Unlimited() || profile.AutoTMM || profile.Forced || dir != want {
			return ErrResponse
		}
	}
	p, err := c.transferProtocol(ctx)
	if err != nil {
		return err
	}
	endpoint := "torrents/recheck"
	if start {
		endpoint = "torrents/resume"
		if p.Major == 5 {
			endpoint = "torrents/start"
		}
	}
	raw, err := c.request(ctx, http.MethodPost, endpoint, url.Values{"hashes": {expected.WireID}})
	if err == nil && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "Ok." {
		return ErrResponse
	}
	return err
}
func (c *Client) RecheckTransfer(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata) error {
	return c.transferAction(ctx, expected, m, false)
}
func (c *Client) StartTransfer(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata) error {
	return c.transferAction(ctx, expected, m, true)
}

func (c *Client) TransferWireID(ctx context.Context, m torrentmeta.TransferMetadata) (string, error) {
	p, err := c.TransferCapabilities(ctx)
	if err != nil {
		return "", err
	}
	return p.WireID(m)
}

var _ torrenttransfer.ReceiverClient = (*Client)(nil)
