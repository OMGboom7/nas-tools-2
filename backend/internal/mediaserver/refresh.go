package mediaserver

// A refresh is a server-side scan, not a local inventory update. Planning only
// reads the configured server. Every mutating request needs a durable intent
// and a caller-side authority check; this client never retries another verb.
import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

const RefreshItemLimit = 1000
const RefreshTargetLimit = 4096

type RefreshItem struct {
	Title      string `json:"title"`
	Year       string `json:"year"`
	Type       string `json:"type"`
	Category   string `json:"category"`
	TMDBID     string `json:"tmdbid"`
	TargetRoot string `json:"targetRoot"`
	TargetPath string `json:"targetPath"`
	Directory  bool   `json:"directory"`
}
type RefreshTarget struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Path  string `json:"path"`
	Label string `json:"label"`
}
type RefreshPlan struct {
	ServerID string          `json:"serverId"`
	Server   string          `json:"server"`
	Targets  []RefreshTarget `json:"targets"`
}

func RefreshPath(value string) (string, bool) {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	value = strings.ReplaceAll(value, "\\", "/")
	drive := len(value) > 2 && value[1:3] == ":/"
	if !strings.HasPrefix(value, "/") && !drive {
		return "", false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", false
		}
	}
	return path.Clean(value), true
}
func refreshContains(base, value string) bool {
	base, ok := RefreshPath(base)
	if !ok {
		return false
	}
	value, ok = RefreshPath(value)
	if !ok {
		return false
	}
	if len(base) > 2 && base[1:3] == ":/" {
		base, value = strings.ToLower(base), strings.ToLower(value)
	}
	return value == base || strings.HasPrefix(value, strings.TrimRight(base, "/")+"/")
}
func ValidRefreshItems(items []RefreshItem) bool {
	if len(items) == 0 || len(items) > RefreshItemLimit {
		return false
	}
	for _, item := range items {
		_, a := RefreshPath(item.TargetRoot)
		_, b := RefreshPath(item.TargetPath)
		if !a || !b || !refreshContains(item.TargetRoot, item.TargetPath) || item.Title == "" || len(item.Title) > 1024 || strings.ContainsAny(item.Title+item.Category, "\x00\r\n") || len(item.Category) > 512 || len(item.TMDBID) > 32 || item.Type != "MOV" && item.Type != "TV" && item.Type != "ANIME" {
			return false
		}
		if item.Year != "" {
			n, err := strconv.Atoi(item.Year)
			if err != nil || n < 1 || n > 9999 {
				return false
			}
		}
	}
	return true
}
func (client *Client) RefreshIdentity(ctx context.Context) (string, error) {
	if client.kind == "plex" {
		b, err := client.plexGET(ctx, "/", nil)
		if err != nil {
			return "", err
		}
		id, err := parsePlexMachineID(b)
		if err != nil || !validInventoryID(id) {
			return "", ErrResponse
		}
		return id, nil
	}
	var info struct {
		ID string `json:"Id"`
	}
	if err := client.readJSON(ctx, client.endpoint.String(), &info); err != nil {
		return "", err
	}
	if !validInventoryID(info.ID) {
		return "", ErrResponse
	}
	return info.ID, nil
}
func (client *Client) PlanRefresh(ctx context.Context, items []RefreshItem, whole bool) (RefreshPlan, error) {
	p := RefreshPlan{Server: client.kind, Targets: []RefreshTarget{}}
	if !whole && !ValidRefreshItems(items) || whole && len(items) != 0 {
		return p, ErrConfiguration
	}
	id, err := client.RefreshIdentity(ctx)
	if err != nil {
		return p, err
	}
	p.ServerID = id
	if client.kind == "plex" {
		p.Targets, err = client.plexRefreshTargets(ctx, items, whole)
		if err == nil && !ValidRefreshPlan(p) {
			return p, ErrResponse
		}
		return p, err
	}
	if whole || client.kind == "jellyfin" {
		p.Targets = []RefreshTarget{{Kind: "root", ID: "root", Label: "整库扫描"}}
		return p, nil
	}
	matchesCache := map[string][]string{}
	targetsSeen := map[string]bool{}
	appendTarget := func(t RefreshTarget) error {
		key := t.Kind + "\x00" + t.ID + "\x00" + t.Path
		if targetsSeen[key] {
			return nil
		}
		if len(p.Targets) >= RefreshTargetLimit {
			return ErrResponse
		}
		targetsSeen[key] = true
		p.Targets = append(p.Targets, t)
		return nil
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		key := strings.Join([]string{item.Type, item.Title, item.Year, item.TMDBID}, "\x00")
		matches, cached := matchesCache[key]
		var err error
		if !cached {
			matches, err = client.embyRefreshMatches(ctx, item)
			if err == nil {
				matchesCache[key] = matches
			}
		}
		if err != nil {
			return p, err
		}
		if len(matches) > 0 {
			if item.Type != "MOV" {
				for _, id := range matches {
					if err := appendTarget(RefreshTarget{Kind: "item", ID: id, Label: item.Title}); err != nil {
						return p, err
					}
				}
			}
			continue // Existing movies retain the old no-refresh behavior.
		}
		target, err := client.embyRefreshFolder(ctx, item)
		if err != nil {
			return p, err
		}
		if target.Kind == "root" {
			p.Targets = []RefreshTarget{target}
			return p, nil
		}
		if err := appendTarget(target); err != nil {
			return p, err
		}
	}
	p.Targets = uniqueRefreshTargets(p.Targets)
	if !ValidRefreshPlan(p) {
		return p, ErrResponse
	}
	return p, nil
}
func uniqueRefreshTargets(v []RefreshTarget) []RefreshTarget {
	out, seen := []RefreshTarget{}, map[string]bool{}
	for _, t := range v {
		key := t.Kind + "\x00" + t.ID + "\x00" + t.Path
		if !seen[key] {
			seen[key] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Kind+out[i].ID+"\x00"+out[i].Path < out[j].Kind+out[j].ID+"\x00"+out[j].Path
	})
	return out
}
func (client *Client) embyRefreshMatches(ctx context.Context, item RefreshItem) ([]string, error) {
	base := *client.endpoint
	base.Path = strings.TrimSuffix(base.Path, "/System/Info") + "/Items"
	kind := "Series"
	if item.Type == "MOV" {
		kind = "Movie"
	}
	result, seen := []string{}, map[string]bool{}
	total := -1
	for offset := 0; offset < 10000; {
		base.RawQuery = url.Values{"IncludeItemTypes": {kind}, "Fields": {"ProductionYear,ProviderIds"}, "Recursive": {"true"}, "SearchTerm": {item.Title}, "IncludeSearchTypes": {"false"}, "Limit": {"100"}, "StartIndex": {strconv.Itoa(offset)}}.Encode()
		var page struct {
			Items []syncJSONItem
			Total *int `json:"TotalRecordCount"`
		}
		if err := client.readJSON(ctx, base.String(), &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.Total == nil || *page.Total < offset+len(page.Items) || *page.Total > 10000 || len(page.Items) > 100 || total != -1 && total != *page.Total {
			return nil, ErrResponse
		}
		total = *page.Total
		for _, v := range page.Items {
			if !validInventoryID(v.ID) || seen[v.ID] {
				return nil, ErrResponse
			}
			seen[v.ID] = true
			if v.Name != item.Title || item.Year != "" && strconv.Itoa(v.Year) != item.Year {
				continue
			}
			tmdb := ""
			for k, id := range v.Providers {
				if strings.EqualFold(k, "tmdb") {
					tmdb = id
				}
			}
			if tmdb != "" && item.TMDBID != "" && tmdb != item.TMDBID {
				continue
			}
			result = append(result, v.ID)
		}
		offset += len(page.Items)
		if offset == total {
			return result, nil
		}
		if len(page.Items) == 0 {
			return nil, ErrResponse
		}
	}
	return nil, ErrResponse
}
func (client *Client) embyRefreshFolder(ctx context.Context, item RefreshItem) (RefreshTarget, error) {
	root := RefreshTarget{Kind: "root", ID: "root", Label: "未命中分类目录，整库扫描"}
	if item.Category == "" {
		return root, nil
	}
	base := *client.endpoint
	base.Path = strings.TrimSuffix(base.Path, "/System/Info") + "/Library/SelectableMediaFolders"
	type sub struct{ ID, Path string }
	var folders []struct {
		ID         string `json:"Id"`
		Name       string
		SubFolders []sub
	}
	if err := client.readJSON(ctx, base.String(), &folders); err != nil {
		return root, err
	}
	if folders == nil || len(folders) > 256 {
		return root, ErrResponse
	}
	candidates, fallbacks := []RefreshTarget{}, []RefreshTarget{}
	for _, f := range folders {
		if !validInventoryID(f.ID) || len(f.SubFolders) > 4096 {
			return root, ErrResponse
		}
		direct := []RefreshTarget{}
		fallback := false
		for _, s := range f.SubFolders {
			p, ok := RefreshPath(s.Path)
			if !ok || !validInventoryID(s.ID) {
				return root, ErrResponse
			}
			if path.Base(p) == item.Category && refreshContains(item.TargetRoot, p) {
				direct = append(direct, RefreshTarget{Kind: "item", ID: s.ID, Path: p, Label: f.Name})
			}
			for _, part := range strings.Split(p, "/") {
				fallback = fallback || part == item.Category
			}
		}
		if len(direct) == 1 {
			candidates = append(candidates, direct[0])
		} else if len(direct) > 1 {
			candidates = append(candidates, RefreshTarget{Kind: "item", ID: f.ID, Label: f.Name})
		} else if fallback {
			fallbacks = append(fallbacks, RefreshTarget{Kind: "item", ID: f.ID, Label: f.Name})
		}
	}
	if len(candidates) == 0 {
		candidates = fallbacks
	}
	candidates = uniqueRefreshTargets(candidates)
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return root, nil // Ambiguous mapping is visible as a full-scan fallback.
}

type plexRefreshSection struct {
	Key      string `json:"key" xml:"key,attr"`
	Title    string `json:"title" xml:"title,attr"`
	Location []struct {
		Path string `json:"path" xml:"path,attr"`
	} `json:"Location" xml:"Location"`
}

func (client *Client) plexRefreshTargets(ctx context.Context, items []RefreshItem, whole bool) ([]RefreshTarget, error) {
	b, err := client.plexGET(ctx, "/library/sections", nil)
	if err != nil {
		return nil, err
	}
	var sections []plexRefreshSection
	if strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		var v struct {
			Container *struct {
				Size  *int                 `json:"size"`
				Total *int                 `json:"totalSize"`
				Items []plexRefreshSection `json:"Directory"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(b, &v) != nil || v.Container == nil || v.Container.Size == nil || *v.Container.Size != len(v.Container.Items) || v.Container.Total != nil && *v.Container.Total != len(v.Container.Items) {
			return nil, ErrResponse
		}
		sections = v.Container.Items
	} else {
		var v struct {
			XMLName xml.Name
			Size    *int                 `xml:"size,attr"`
			Total   *int                 `xml:"totalSize,attr"`
			Items   []plexRefreshSection `xml:"Directory"`
		}
		if xml.Unmarshal(b, &v) != nil || v.XMLName.Local != "MediaContainer" || v.Size == nil || *v.Size != len(v.Items) || v.Total != nil && *v.Total != len(v.Items) {
			return nil, ErrResponse
		}
		sections = v.Items
	}
	if len(sections) > 256 {
		return nil, ErrResponse
	}
	all, seen := []RefreshTarget{}, map[string]bool{}
	for _, section := range sections {
		n, err := strconv.ParseInt(section.Key, 10, 64)
		if err != nil || n < 1 || seen[section.Key] || len(section.Location) > 256 {
			return nil, ErrResponse
		}
		seen[section.Key] = true
		for _, loc := range section.Location {
			if _, ok := RefreshPath(loc.Path); !ok {
				return nil, ErrResponse
			}
		}
		all = append(all, RefreshTarget{Kind: "section", ID: section.Key, Label: section.Title})
	}
	if whole {
		return all, nil
	}
	result := []RefreshTarget{}
	for _, item := range items {
		p, _ := RefreshPath(item.TargetPath)
		if !item.Directory {
			p = path.Dir(p)
		}
		best, length, ambiguous := RefreshTarget{}, -1, false
		for _, section := range sections {
			for _, loc := range section.Location {
				base, _ := RefreshPath(loc.Path)
				if !refreshContains(base, p) {
					continue
				}
				if len(base) > length {
					best, length, ambiguous = RefreshTarget{Kind: "section", ID: section.Key, Path: p, Label: section.Title}, len(base), false
				} else if len(base) == length && section.Key != best.ID {
					ambiguous = true
				}
			}
		}
		if length < 0 || ambiguous {
			return all, nil
		}
		result = append(result, best)
	}
	return uniqueRefreshTargets(result), nil
}
func (client *Client) SendRefresh(ctx context.Context, target RefreshTarget) error {
	u := *client.endpoint
	query := url.Values{}
	if client.kind == "plex" {
		n, err := strconv.ParseInt(target.ID, 10, 64)
		if target.Kind != "section" || err != nil || n < 1 {
			return ErrConfiguration
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/library/sections/" + target.ID + "/refresh"
		if target.Path != "" {
			if _, ok := RefreshPath(target.Path); !ok {
				return ErrConfiguration
			}
			query.Set("path", target.Path)
		}
	} else {
		u.Path = strings.TrimSuffix(u.Path, "/System/Info")
		if target.Kind == "root" && target.ID == "root" {
			u.Path += "/Library/Refresh"
		} else if client.kind == "emby" && target.Kind == "item" && validInventoryID(target.ID) {
			u.Path += "/Items/" + target.ID + "/Refresh"
			query = url.Values{"Recursive": {"true"}, "MetadataRefreshMode": {"Default"}, "ImageRefreshMode": {"Default"}, "ReplaceAllMetadata": {"false"}, "ReplaceAllImages": {"false"}}
		} else {
			return ErrConfiguration
		}
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return ErrConfiguration
	}
	req.Header.Set("Accept", "application/json")
	if client.kind == "plex" {
		req.Header.Set("X-Plex-Token", client.credential)
	} else {
		req.Header.Set("X-Emby-Token", client.credential)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return ErrConnection
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 && resp.StatusCode != 204 {
		return ErrResponse
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); err != nil {
		return ErrResponse
	}
	return nil
}
