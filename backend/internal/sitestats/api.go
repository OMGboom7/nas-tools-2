package sitestats

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

func parseTNode(ctx context.Context, f Fetcher, s Snapshot, page string, o Options) (Snapshot, error) {
	token := firstAttr(page, `meta[name="x-csrf-token"]`, "content")
	if token == "" {
		return s, ErrLogin
	}
	headers := map[string]string{"X-CSRF-TOKEN": token}
	b, e := f.Fetch(ctx, Request{Path: "api/user/getMainInfo", Raw: true, Headers: headers})
	if e != nil {
		return s, e
	}
	v, e := decode(b)
	if e != nil || integer(v["status"]) != 200 {
		return s, ErrLogin
	}
	m := object(v["data"])
	if !validIntegers(m, "upload", "download") || str(m["id"]) == "" || str(m["username"]) == "" {
		return s, ErrResponse
	}
	s.UserID = str(m["id"])
	s.Username = str(m["username"])
	s.Level = str(object(m["class"])["name"])
	s.Joined = date(str(m["regTime"]))
	s.Upload = integer(m["upload"])
	s.Download = integer(m["download"])
	s.Bonus = number(str(m["bonus"]))
	s.Unread = integer(m["unreadAdmin"]) + integer(m["unreadInbox"]) + integer(m["unreadSystem"])
	if s.Download > 0 {
		s.Ratio = float64(s.Upload) / float64(s.Download)
	}
	for pageNum := 1; pageNum <= MaxPages; pageNum++ {
		b, e = f.Fetch(ctx, Request{Path: "api/user/listTorrentActivity?id=&type=seeding&size=100&page=" + strconv.Itoa(pageNum), Raw: true, Headers: headers})
		if e != nil {
			return s, e
		}
		v, e = decode(b)
		if e != nil || integer(v["status"]) != 200 {
			return s, ErrResponse
		}
		m = object(v["data"])
		torrents, ok := m["torrents"].([]any)
		if !ok || !validIntegers(m, "total") {
			return s, ErrResponse
		}
		total := integer(m["total"])
		for _, raw := range torrents {
			m := object(raw)
			if !validIntegers(m, "size", "seeding") {
				return s, ErrResponse
			}
			n := integer(m["size"])
			if e = addSeed(&s, integer(m["seeding"]), n); e != nil {
				return s, e
			}
		}
		if s.Seeding == total {
			s.SeedDetailsKnown = true
			parseFavicon(ctx, f, &s, page)
			return s, nil
		}
		if s.Seeding > total || len(torrents) == 0 {
			return s, ErrResponse
		}
	}
	return s, ErrPage
}

var mteamRoles = map[string]string{"1": "User", "2": "Power User", "3": "Elite User", "4": "Crazy User", "5": "Insane User", "6": "Veteran User", "7": "Extreme User", "8": "Ultimate User", "9": "Nexus Master", "10": "VIP", "17": "Offer memberStaff", "18": "bet memberStaff", "12": "巡查", "11": "職人", "13": "總版", "14": "總管", "15": "維護開發員", "16": "站長"}

func parseMTeam(ctx context.Context, f Fetcher, s Snapshot, o Options) (Snapshot, error) {
	if o.MTeamAPI == "" {
		return s, ErrOrigin
	}
	base := strings.TrimRight(o.MTeamAPI, "/")
	b, e := f.Fetch(ctx, Request{Path: base + "/api/member/profile", JSON: map[string]any{}})
	if e != nil {
		return s, e
	}
	v, e := decode(b)
	if e != nil {
		return s, e
	}
	m := object(v["data"])
	count := object(m["memberCount"])
	if !validIntegers(count, "uploaded", "downloaded") || str(m["id"]) == "" || str(m["username"]) == "" {
		return s, ErrLogin
	}
	s.UserID = str(m["id"])
	s.Username = str(m["username"])
	s.Level = mteamRoles[str(m["role"])]
	s.Joined = date(str(m["createdDate"]))
	s.LastSeen = date(str(object(m["memberStatus"])["lastBrowse"]))
	s.Upload = integer(count["uploaded"])
	s.Download = integer(count["downloaded"])
	s.Ratio = number(str(count["shareRate"]))
	s.Bonus = number(str(count["bonus"]))
	if s.Download > 0 && s.Ratio == 0 {
		s.Ratio = float64(s.Upload) / float64(s.Download)
	}
	var total int64 = -1
	for page := 1; page <= MaxPages; page++ {
		b, e = f.Fetch(ctx, Request{Path: base + "/api/member/getUserTorrentList", JSON: map[string]any{"pageNumber": page, "pageSize": 200, "userid": s.UserID, "type": "SEEDING"}})
		if e != nil {
			return s, e
		}
		v, e = decode(b)
		if e != nil {
			return s, e
		}
		m = object(v["data"])
		torrents, ok := m["data"].([]any)
		if !ok || !validIntegers(m, "total", "totalPages") {
			return s, ErrResponse
		}
		if total < 0 {
			total = integer(m["total"])
		} else if total != integer(m["total"]) {
			return s, ErrResponse
		}
		for _, raw := range torrents {
			t := object(object(raw)["torrent"])
			status := object(t["status"])
			if !validIntegers(t, "size") || !validIntegers(status, "seeders") {
				return s, ErrResponse
			}
			n := integer(t["size"])
			if e = addSeed(&s, integer(status["seeders"]), n); e != nil {
				return s, e
			}
		}
		if s.Seeding == total {
			s.SeedDetailsKnown = true
			return s, nil
		}
		if len(torrents) == 0 || s.Seeding > total || int64(page) >= integer(m["totalPages"]) {
			return s, ErrResponse
		}
	}
	return s, ErrPage
}

// Resolve never promotes a page-provided link to a new credential authority.
func Resolve(base, path string) (*url.URL, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrOrigin
	}
	ref, e := url.Parse(path)
	if e != nil {
		return nil, ErrOrigin
	}
	out := u.ResolveReference(ref)
	if !strings.EqualFold(u.Host, out.Host) || u.Scheme != out.Scheme || out.User != nil || out.Fragment != "" {
		return nil, ErrOrigin
	}
	return out, nil
}
