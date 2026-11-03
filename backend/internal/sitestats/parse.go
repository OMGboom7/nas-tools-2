package sitestats

import (
	"context"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Parse consumes a complete site, including all required detail and seed pages.
// A caller must publish the snapshot only after this method succeeds.
func Parse(ctx context.Context, f Fetcher, home []byte, o Options) (Snapshot, error) {
	page := string(home)
	s := Snapshot{Site: o.Site, URL: o.URL, Schema: Identify(page), Seeds: [][2]int64{}, Messages: []Message{}}
	if s.Schema == "Mteam" {
		return parseMTeam(ctx, f, s, o)
	}
	if len(nodes(page, `input[type="password"]`)) > 0 {
		return s, ErrLogin
	}
	if s.Schema == "TNode" {
		return parseTNode(ctx, f, s, page, o)
	}
	detail, seed := "", ""
	switch s.Schema {
	case "Gazelle", "SmallHorse":
		s.UserID = capture(firstAttr(page, `a[href*="user.php?id="]`, "href"), `[?&]id=(\d+)`)
		s.Username = first(page, `a[href*="user.php?id="]`)
		detail = "user.php?id=" + s.UserID
		seed = "torrents.php?type=seeding&userid=" + s.UserID
	case "Discuz":
		detail = firstAttr(page, `a[href*="&uid="]`, "href")
		s.UserID = capture(detail, `[?&]uid=(\d+)`)
		s.Username = first(page, `a[href*="&uid="]`)
		seed = "forum.php?&mod=torrents&cat_5up=on"
	case "Unit3d":
		s.Username = capture(firstAttr(page, `a[href*="/users/"][href*="settings"]`, "href"), `/users/([^/]+)/settings`)
		s.UserID = s.Username
		detail = "/users/" + url.PathEscape(s.Username)
		seed = detail + "/active?perPage=100&client=&seeding=include"
	case "TorrentLeech":
		s.Username = capture(firstAttr(page, `a[href*="/profile/"]`, "href"), `/profile/([^/]+)/`)
		s.UserID = s.Username
		detail = "profile/" + url.PathEscape(s.Username) + "/view"
		seed = "profile/" + url.PathEscape(s.Username) + "/seeding"
	case "Ipt":
		s.UserID = capture(firstAttr(page, `a[href*="/u/"]`, "href"), `/u/(\d+)`)
		ns := nodes(page, `a[href*="/u/"]`)
		if len(ns) > 0 {
			s.Username = text(ns[len(ns)-1])
		}
		detail = "user.php?u=" + s.UserID
		seed = "peers?u=" + s.UserID
	default:
		detail = firstAttr(page, `a[href*="userdetails.php?id="]`, "href")
		s.UserID = capture(detail, `[?&]id=(\d+)`)
		s.Username = first(page, `a[href*="userdetails.php?id="]`)
		seed = "getusertorrentlistajax.php?userid=" + s.UserID + "&type=seeding"
		if s.Schema == "NexusProject" {
			seed = "viewusertorrents.php?id=" + s.UserID + "&show=seeding"
		}
		if s.Schema == "FileList" {
			seed = "snatchlist.php?id=" + s.UserID + "&action=torrents&type=seeding"
		}
		if s.Schema == "NexusRabbit" {
			seed = "getusertorrentlistajax.php?page=1&limit=500&type=seeding&uid=" + s.UserID
		}
		if detail == "" && len(nodes(page, `a[href*="userdetails"]`)) > 0 {
			detail = firstAttr(page, `a[href*="userdetails"]`, "href")
			seed = ""
			s.UserID = "self"
			s.Username = first(page, `a[href*="userdetails"]`)
		}
	}
	if s.UserID == "" || detail == "" {
		return s, ErrLogin
	}
	parseProfile(&s, page)
	// Message retrieval retains the original order and may mark remote messages read.
	if o.ReadMessages && s.Unread > 0 && isNexus(s.Schema) {
		if e := parseMessages(ctx, f, &s); e != nil {
			return s, e
		}
	}
	d, e := f.Fetch(ctx, Request{Path: detail})
	if e != nil {
		return s, e
	}
	if len(nodes(string(d), `input[type="password"]`)) > 0 {
		return s, ErrLogin
	}
	unread := s.Unread
	parseProfile(&s, string(d))
	// Detail requests follow message reads; retain the pre-read unread count.
	s.Unread = unread
	if !s.uploadFound || !s.downloadFound {
		return s, ErrResponse
	}
	if s.Download > 0 && s.Ratio == 0 {
		s.Ratio = float64(s.Upload) / float64(s.Download)
	}
	if isNexus(s.Schema) {
		if v := firstAttr(string(d), `a[href*="getusertorrentlist.php"][href*="seeding"]`, "href"); v != "" {
			seed = v
		}
		if seed == "" {
			id := capture(string(d), `getusertorrentlistajax\(\s*'(\d+)`)
			if id != "" {
				s.UserID = id
				seed = "getusertorrentlistajax.php?userid=" + id + "&type=seeding"
			}
		}
	}
	if seed != "" {
		request := Request{Path: seed}
		if isNexus(s.Schema) && contains(string(d), "javascript: getusertorrentlistajax") {
			if csrf := firstAttr(string(d), `meta[name="x-csrf"]`, "content"); csrf != "" {
				request.Path = "ajax_getusertorrentlist.php"
				request.Form = url.Values{"userid": {s.UserID}, "type": {"seeding"}, "csrf": {csrf}}
			}
		}
		if s.Schema == "NexusRabbit" {
			s.SeedDetailsKnown = true
			if e := parseRabbit(ctx, f, &s, seed); e != nil {
				return s, e
			}
		} else if e := seedPages(ctx, f, &s, request); e != nil {
			return s, e
		}
	} else if isNexus(s.Schema) {
		if !parseNexusSeedFallback(&s, string(d)) {
			return s, ErrResponse
		}
	}
	parseFavicon(ctx, f, &s, page)
	return s, nil
}

func isNexus(s string) bool { return s == "NexusPhp" || s == "NexusProject" || s == "NexusRabbit" }
func parseFavicon(ctx context.Context, f Fetcher, s *Snapshot, page string) {
	path := firstAttr(page, `link[rel*="icon"]`, "href")
	if path == "" {
		path = "/favicon.ico"
	}
	if b, e := f.Fetch(ctx, Request{Path: path, Raw: true}); e == nil && len(b) > 0 && len(b) <= 128<<10 {
		s.Favicon = base64.StdEncoding.EncodeToString(b)
	}
}
func parseProfile(s *Snapshot, page string) {
	traffic(s, page)
	if v := label(page, "Uploaded", "上传量", "上傳量"); v != "" {
		assignSize(s, true, v)
	}
	if v := label(page, "Downloaded", "下载量", "下載量"); v != "" {
		assignSize(s, false, v)
	}
	if v := label(page, "Class", "等级", "等級"); v != "" {
		s.Level = v
		if n := valueNode(page, "等级", "等級"); n != nil {
			for _, img := range query(n, "img[title]") {
				s.Level = attr(img, "title")
				break
			}
		}
	}
	if v := label(page, "Join date", "Join date:", "Join Date", "Registration date", "加入日期", "注册日期"); v != "" {
		s.Joined = date(v)
	}
	if v := label(page, "最近动向", "最近動向"); v != "" {
		s.LastSeen = date(v)
	}
	for _, sel := range []string{`a[href*="mybonus"]`, `a[href*="bonus.php"]`, `a[href*="bonus/earnings"]`, `a[href*="shop.php"]`, `.total-TL-points`} {
		if v := first(page, sel); capture(v, `([\d,.]+)`) != "" {
			s.Bonus = number(v)
			break
		}
	}
	if v := label(page, "魔力值", "猫粮", "积分"); v != "" {
		s.Bonus = number(v)
	}
	gold, silver, copper := first(page, ".ucoin-gold"), first(page, ".ucoin-silver"), first(page, ".ucoin-copper")
	if gold != "" || silver != "" || copper != "" {
		s.Bonus = number(gold)*10000 + number(silver)*100 + number(copper)
	}
	if isNexus(s.Schema) {
		for _, a := range nodes(page, `a[href*="messages.php"]`) {
			v := text(a.Parent)
			n := capture(v, `(?:信息箱\s*|\(|你有\s+)(\d+)`)
			if n == "" {
				n = capture(text(a), `^(\d+)$`)
			}
			if n != "" {
				s.Unread = int64(number(n))
				break
			}
		}
		if s.Level == "" {
			s.Level = capture(first(page, `a[href*="userdetails"]`), `\[([^\]]+)\]`)
		}
	}
	switch s.Schema {
	case "Gazelle":
		for _, v := range []struct {
			sel string
			up  bool
		}{{"#header-uploaded-value", true}, {"#header-downloaded-value", false}} {
			if value := firstAttr(page, v.sel, "data-value"); value != "" {
				assignSize(s, v.up, value)
			}
		}
		if !s.uploadFound {
			assignSize(s, true, first(page, "#stats_seeding span"))
		}
		if !s.downloadFound {
			assignSize(s, false, first(page, "#stats_leeching span"))
		}
		if v := firstAttr(page, "#class-value", "data-value"); v != "" {
			s.Level = v
		}
		if v := firstAttr(page, "#join-date-value", "data-value"); v != "" {
			s.Joined = date(v)
		}
		if v := firstAttr(page, `a[href*="bonus.php"]`, "data-tooltip"); v != "" {
			s.Bonus = number(v)
		}
		for _, li := range nodes(page, "li") {
			v := text(li)
			if contains(v, "用户等级") {
				s.Level = strings.TrimSpace(strings.TrimPrefix(v, "用户等级:"))
			}
			if contains(v, "加入时间") {
				s.Joined = date(text(firstNode(query(li, "span"))))
			}
		}
	case "TorrentLeech":
		assignSize(s, true, first(page, ".profile-uploaded span"))
		assignSize(s, false, first(page, ".profile-downloaded span"))
		if v := first(page, ".profile-ratio span"); v != "" {
			s.Ratio = number(v)
		}
	case "Unit3d":
		if v := first(page, ".content .badge-user"); v != "" {
			s.Level = v
		}
		for _, h := range nodes(page, ".content h4") {
			v := text(h)
			for _, key := range []string{"注册日期", "註冊日期", "Registration date"} {
				if strings.HasPrefix(v, key) {
					s.Joined = date(strings.TrimLeft(strings.TrimPrefix(v, key), ":： "))
				}
			}
		}
	case "SmallHorse":
		for _, li := range nodes(page, "ul.stats li") {
			v := text(li)
			if contains(v, "Joined:", "加入时间") {
				s.Joined = date(text(firstNode(query(li, "span"))))
			}
			if strings.HasPrefix(v, "Class:") {
				s.Level = strings.TrimSpace(strings.TrimPrefix(v, "Class:"))
			}
			if strings.HasPrefix(v, "Bonus:") {
				s.Bonus = number(strings.TrimPrefix(v, "Bonus:"))
			}
			if strings.HasPrefix(v, "Leeching:") {
				s.Leeching = int64(number(strings.TrimPrefix(v, "Leeching:")))
			}
		}
	case "Discuz":
		levels := nodes(page, `a[href*="usergroup"]`)
		if len(levels) > 0 {
			s.Level = text(levels[len(levels)-1])
		}
		for _, li := range nodes(page, "li") {
			key := text(firstNode(query(li, "em")))
			v := strings.TrimSpace(strings.TrimPrefix(text(li), key))
			switch key {
			case "注册时间":
				s.Joined = date(v)
			case "积分":
				s.Bonus = number(v)
			case "上传量":
				assignSize(s, true, lastSlash(v))
			case "下载量":
				assignSize(s, false, lastSlash(v))
			}
		}
	case "Ipt":
		blocks := nodes(page, "div.stats > div > div")
		if len(blocks) > 0 {
			spans := query(blocks[0], "span")
			if len(spans) >= 3 {
				s.Ratio = number(text(spans[0]))
				assignSize(s, true, text(spans[1]))
				assignSize(s, false, text(spans[2]))
			}
			a := query(blocks[0], "a")
			if len(a) >= 4 {
				s.Bonus = number(text(a[3]))
				s.Leeching = int64(number(capture(text(a[2]), `\d+\D+(\d+)`)))
			}
		}
	}
}
func firstNode(ns []*html.Node) *html.Node {
	if len(ns) == 0 {
		return nil
	}
	return ns[0]
}
func lastSlash(s string) string { parts := strings.Split(s, "/"); return parts[len(parts)-1] }

func parseMessages(ctx context.Context, f Fetcher, s *Snapshot) error {
	links := []string{}
	seenLinks := map[string]bool{}
	for _, start := range []string{"messages.php?action=viewmailbox&box=1&unread=yes", "messages.php?action=viewmailbox&box=-2&unread=yes"} {
		path := start
		visited := map[string]bool{}
		for n := 0; path != ""; n++ {
			if n >= MaxPages || visited[path] {
				return ErrPage
			}
			visited[path] = true
			b, e := f.Fetch(ctx, Request{Path: path})
			if e != nil {
				return e
			}
			p := string(b)
			for _, row := range nodes(p, "tr") {
				if len(query(row, `img[alt="Read"]`)) > 0 {
					continue
				}
				for _, a := range query(row, `td a[href*="viewmessage"]`) {
					href := attr(a, "href")
					if !seenLinks[href] {
						seenLinks[href] = true
						links = append(links, href)
						if len(links) > 2000 {
							return ErrPage
						}
					}
				}
			}
			path = pageReference(path, nextPage(p))
		}
	}
	for _, path := range links {
		b, e := f.Fetch(ctx, Request{Path: path, Message: true})
		if e != nil {
			return e
		}
		p := string(b)
		msg := Message{Title: first(p, "h1"), Content: first(p, ".layui-card-body")}
		headers := nodes(p, ".layui-card-header > span")
		if len(headers) > 0 {
			msg.Title = text(headers[0])
		}
		if len(headers) > 1 {
			msg.Date = text(headers[1])
		}
		if msg.Content == "" {
			for _, table := range nodes(p, "table") {
				if len(query(table, "td.colhead")) == 0 {
					continue
				}
				rows := query(table, "tr")
				if len(rows) > 1 {
					cells := query(rows[1], "td")
					if len(cells) > 1 {
						msg.Date = text(cells[1])
					}
				}
				if len(rows) > 2 {
					msg.Content = text(firstNode(query(rows[2], "td")))
				}
				break
			}
		}
		if msg.Title == "" || msg.Content == "" {
			return ErrResponse
		}
		s.Messages = append(s.Messages, msg)
	}
	return nil
}
func nextPage(page string) string {
	for _, a := range nodes(page, "a[href]") {
		v := strings.TrimSpace(text(a))
		if v == "Next" || v == "Next →" || v == ">" || contains(v, "下一页", "下一頁") {
			return attr(a, "href")
		}
	}
	for _, li := range nodes(page, "ul.pagination li.active") {
		if n := nextSibling(li); n != nil {
			if a := firstNode(query(n, "a[href]")); a != nil {
				return attr(a, "href")
			}
		}
	}
	return ""
}
func seedPages(ctx context.Context, f Fetcher, s *Snapshot, r Request) error {
	s.SeedDetailsKnown = true
	seen := map[string]bool{}
	s.Seeds = [][2]int64{}
	s.Seeding = 0
	s.SeedingSize = 0
	for page := 0; r.Path != ""; page++ {
		if page >= MaxPages || seen[r.Path] {
			return ErrPage
		}
		seen[r.Path] = true
		b, e := f.Fetch(ctx, r)
		if e != nil {
			return e
		}
		p := strings.ReplaceAll(string(b), `\/`, `/`)
		if len(nodes(p, `input[type="password"]`)) > 0 {
			return ErrLogin
		}
		if page == 0 && isNexus(s.Schema) {
			if link := firstAttr(p, `a[href*="torrents.php"][href*="seeding"]`, "href"); link != "" {
				r.Path = link
				r.Form = nil
				continue
			}
		}
		rows, valid := seedRows(p, s.Schema)
		if !valid {
			return ErrResponse
		}
		for _, v := range rows {
			if e = addSeed(s, v[0], v[1]); e != nil {
				return e
			}
		}
		r.Path = pageReference(r.Path, nextPage(p))
		if r.Path != "" && isNexus(s.Schema) {
			u, e := url.Parse(r.Path)
			if e != nil {
				return ErrPage
			}
			q := u.Query()
			if q.Get("userid") == "" {
				q.Set("userid", s.UserID)
				q.Set("type", "seeding")
			}
			u.RawQuery = q.Encode()
			r.Path = u.String()
		}
	}
	return nil
}

func seedRows(page, schema string) ([][2]int64, bool) {
	out := [][2]int64{}
	selector := "table"
	sizeCol, seedCol := 3, 4
	switch schema {
	case "FileList":
		sizeCol, seedCol = 6, 7
	case "SmallHorse":
		selector = "table#torrent_table"
		sizeCol, seedCol = 6, 8
	case "TorrentLeech":
		sizeCol, seedCol = 2, 7
	case "Unit3d":
		sizeCol, seedCol = 9, 2
	case "Ipt":
		sizeCol, seedCol = 6, 0
	case "Gazelle":
		selector = `table[id*="torrent"]`
	}
	if isNexus(schema) && len(nodes(page, "table.torrents")) > 0 {
		selector = "table.torrents"
	}
	tables := nodes(page, selector)
	if len(tables) == 0 {
		return out, contains(strings.ToLower(textFromFragment(page)), "no torrents", "没有种子", "没有做种", "無種子", "没有记录", "no records", "nothing found")
	}
	valid := false
	for _, table := range tables {
		trs := ownedRows(table)
		if len(trs) == 0 {
			continue
		}
		sc, pc := sizeCol, seedCol
		if schema == "Gazelle" {
			cells := query(trs[0], "td,th")
			if len(cells) > 3 {
				sc = len(cells) - 3
				pc = sc + 2
			}
		}
		headerKnown := false
		headerRows := map[*html.Node]bool{}
		for _, header := range trs {
			cells := directCells(header)
			for i, c := range cells {
				v := strings.ToLower(strings.TrimSpace(text(c)))
				if v == "大小" || v == "尺寸" || v == "size" || len(query(c, `img[alt="size"], img.size`)) > 0 || contains(attr(c, "class"), "size") {
					sc = i + 1
					headerKnown = true
					headerRows[header] = true
				}
				if v == "在做种" || v == "做种者" || v == "seeders" || len(query(c, `img[alt="seeders"], img.seeders`)) > 0 || contains(attr(c, "class"), "seeders") {
					pc = i + 1
				}
			}
			if len(cells) > 0 {
				break
			}
		}
		iptSeeding := schema != "Ipt"
		for _, row := range trs {
			cells := directCells(row)
			if len(cells) == 0 {
				continue
			}
			if headerRows[row] {
				valid = true
				continue
			}
			if len(query(row, "table")) > 0 {
				continue
			}
			if schema == "Ipt" {
				head := text(cells[0])
				if head == "Seeders" {
					iptSeeding = true
					valid = true
					continue
				}
				if head == "Leechers" {
					iptSeeding = false
				}
				if !iptSeeding {
					continue
				}
			}
			if len(cells) < sc || len(cells) < pc {
				if headerKnown && !emptySeeds(text(row)) {
					return out, false
				}
				continue
			}
			value := text(cells[sc-1])
			if schema == "Ipt" {
				if v := capture(value, `\(([^)]+)\)`); v != "" {
					value = v
				}
			}
			n, ok := size(value)
			if !ok || capture(value, `(?i)([KMGTPE]?i?B)\b`) == "" {
				if value == "Size" || value == "大小" || value == "尺寸" {
					valid = true
				} else if headerKnown {
					return out, false
				}
				continue
			}
			peers := int64(0)
			if pc > 0 {
				peers = int64(number(text(cells[pc-1])))
			}
			out = append(out, [2]int64{peers, n})
			valid = true
		}
	}
	return out, valid
}

func emptySeeds(value string) bool {
	return contains(strings.ToLower(value), "no torrents", "没有种子", "没有做种", "無種子", "没有记录", "no records", "nothing found", "暂无数据")
}

func pageReference(current, next string) string {
	if next == "" {
		return ""
	}
	base, e := url.Parse(current)
	if e != nil {
		return next
	}
	ref, e := url.Parse(next)
	if e != nil {
		return next
	}
	return base.ResolveReference(ref).String()
}

func ownedRows(table *html.Node) []*html.Node {
	out := []*html.Node{}
	for _, row := range query(table, "tr") {
		for p := row.Parent; p != nil; p = p.Parent {
			if p.Type == html.ElementNode && p.Data == "table" {
				if p == table {
					out = append(out, row)
				}
				break
			}
		}
	}
	return out
}

func parseNexusSeedFallback(s *Snapshot, page string) bool {
	if n := valueNode(page, "当前上传"); n != nil {
		for _, table := range query(n, "table") {
			var b strings.Builder
			_ = html.Render(&b, table)
			rows, ok := seedRows(b.String(), "NexusPhp")
			if !ok {
				continue
			}
			s.Seeds = rows
			s.Seeding = int64(len(rows))
			s.SeedingSize = 0
			for _, v := range rows {
				s.SeedingSize += v[1]
			}
			s.SeedDetailsKnown = true
			return true
		}
	}
	if v := label(page, "做种统计"); v != "" {
		count := capture(v, `总做种数\s*[:：]\s*(\d+)`)
		bytes := capture(v, `总做种体积\s*[:：]\s*([\d,.]+\s*[KMGTPE]?i?B)`)
		if count != "" && bytes != "" {
			if n, ok := size(bytes); ok {
				s.Seeding = int64(number(count))
				s.SeedingSize = n
				return true
			}
		}
	}
	return false
}
func directCells(n *html.Node) []*html.Node {
	out := []*html.Node{}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
			out = append(out, c)
		}
	}
	return out
}

func parseRabbit(ctx context.Context, f Fetcher, s *Snapshot, path string) error {
	for page := 1; page <= MaxPages; page++ {
		u, e := url.Parse(path)
		if e != nil {
			return e
		}
		q := u.Query()
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()
		b, e := f.Fetch(ctx, Request{Path: u.String(), Raw: true, Headers: map[string]string{"Accept": "application/json, text/javascript, */*; q=0.01"}})
		if e != nil {
			return e
		}
		v, e := decode(b)
		if e != nil {
			return e
		}
		raw, ok := v["data"].([]any)
		if !ok {
			return ErrResponse
		}
		for _, item := range raw {
			m := object(item)
			if !validIntegers(m, "size", "seeders") {
				return ErrResponse
			}
			n, p := integer(m["size"]), integer(m["seeders"])
			if e = addSeed(s, p, n); e != nil {
				return e
			}
		}
		total := integer(v["count"])
		if total == 0 {
			total = integer(v["total"])
		}
		if (total > 0 && s.Seeding >= total) || (total == 0 && len(raw) < 500) {
			return nil
		}
		if len(raw) == 0 {
			return ErrResponse
		}
	}
	return ErrPage
}
