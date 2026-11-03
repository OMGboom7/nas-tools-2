package sitestats

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fetchFunc func(context.Context, Request) ([]byte, error)

func (f fetchFunc) Fetch(ctx context.Context, r Request) ([]byte, error) { return f(ctx, r) }

const fixtureProfile = `<table><tr><td>Uploaded</td><td>2 GiB</td></tr><tr><td>Downloaded</td><td>1 GiB</td></tr><tr><td>Class</td><td>Power User</td></tr><tr><td>Join date</td><td>2020-01-02</td></tr></table>`

func seedTable(columns, sizeCol, peersCol int) string {
	var b strings.Builder
	b.WriteString(`<table id="torrent_table" class="torrents"><tr>`)
	for i := 1; i <= columns; i++ {
		v := "Other"
		if i == sizeCol {
			v = "Size"
		}
		if i == peersCol {
			v = "Seeders"
		}
		fmt.Fprintf(&b, "<td>%s</td>", v)
	}
	b.WriteString("</tr><tr>")
	for i := 1; i <= columns; i++ {
		v := "row"
		if i == sizeCol {
			v = "1.5 GiB"
		}
		if i == peersCol {
			v = "3"
		}
		fmt.Fprintf(&b, "<td>%s</td>", v)
	}
	b.WriteString("</tr></table>")
	return b.String()
}

func TestOriginalTwelveAdapters(t *testing.T) {
	for _, tc := range []struct {
		schema, marker, link, detail, seed string
		columns, sizeCol, peersCol         int
	}{
		{"NexusPhp", "", `userdetails.php?id=42`, `userdetails.php?id=42`, `getusertorrentlistajax.php?userid=42&type=seeding`, 5, 3, 4},
		{"NexusProject", "Nexus Project", `userdetails.php?id=42`, `userdetails.php?id=42`, `viewusertorrents.php?id=42&show=seeding`, 5, 3, 4},
		{"NexusRabbit", "Style by Rabbit", `userdetails.php?id=42`, `userdetails.php?id=42`, `getusertorrentlistajax.php`, 0, 0, 0},
		{"FileList", "Powered by FileList", `userdetails.php?id=42`, `userdetails.php?id=42`, `snatchlist.php?id=42&action=torrents&type=seeding`, 8, 6, 7},
		{"Gazelle", "Powered by Gazelle", `user.php?id=42`, `user.php?id=42`, `torrents.php?type=seeding&userid=42`, 8, 5, 7},
		{"SmallHorse", "Small Horse", `user.php?id=42`, `user.php?id=42`, `torrents.php?type=seeding&userid=42`, 9, 6, 8},
		{"Discuz", "Powered by Discuz!", `home.php?mod=space&uid=42`, `home.php?mod=space&uid=42`, `forum.php?&mod=torrents&cat_5up=on`, 6, 3, 4},
		{"Unit3d", `<script src="unit3d.js"></script>`, `/users/fixture/settings`, `/users/fixture`, `/users/fixture/active?perPage=100&client=&seeding=include`, 10, 9, 2},
		{"TorrentLeech", "TorrentLeech", `/profile/fixture/view`, `profile/fixture/view`, `profile/fixture/seeding`, 8, 2, 7},
		{"Ipt", "IPTorrents", `/u/42`, `user.php?u=42`, `peers?u=42`, 8, 6, 0},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			home := `<html><head><title>fixture</title></head><body>` + tc.marker + `<a href="` + tc.link + `">fixture</a></body></html>`
			seeds := seedTable(tc.columns, tc.sizeCol, tc.peersCol)
			if tc.schema == "Ipt" {
				seeds = `<table><tr><td>Seeders</td></tr><tr><td>a</td><td>b</td><td>c</td><td>d</td><td>e</td><td>1.5 GiB</td></tr><tr><td>Leechers</td></tr><tr><td>a</td><td>b</td><td>c</td><td>d</td><td>e</td><td>9 GiB</td></tr></table>`
			}
			f := fetchFunc(func(ctx context.Context, r Request) ([]byte, error) {
				if strings.Contains(r.Path, "favicon") {
					return nil, ErrResponse
				}
				if r.Path == tc.detail {
					return []byte(fixtureProfile), nil
				}
				if tc.schema == "NexusRabbit" && strings.HasPrefix(r.Path, tc.seed) {
					return []byte(`{"data":[{"size":1610612736,"seeders":3}],"count":1}`), nil
				}
				if r.Path == tc.seed {
					return []byte(seeds), nil
				}
				t.Fatalf("unexpected %s", r.Path)
				return nil, ErrResponse
			})
			v, e := Parse(t.Context(), f, []byte(home), Options{Site: "fixture", URL: "https://fixture.invalid"})
			if e != nil {
				t.Fatal(e)
			}
			if v.Schema != tc.schema || v.Upload != 2<<30 || v.Download != 1<<30 || v.Seeding != 1 || v.SeedingSize != 1610612736 || len(v.Seeds) != 1 || v.Level != "Power User" {
				t.Fatalf("snapshot=%+v", v)
			}
		})
	}
	t.Run("TNode", func(t *testing.T) {
		pages := 0
		f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
			if r.Headers["X-CSRF-TOKEN"] != "fixture-csrf" {
				if strings.Contains(r.Path, "favicon") {
					return nil, ErrResponse
				}
				t.Fatal("CSRF lost")
			}
			if r.Path == "api/user/getMainInfo" {
				return []byte(`{"status":200,"data":{"id":42,"username":"fixture","class":{"name":"VIP"},"upload":9007199254740993,"download":1073741824,"bonus":"1.25","regTime":"2020-01-02","unreadAdmin":1,"unreadInbox":2,"unreadSystem":3}}`), nil
			}
			pages++
			return []byte(`{"status":200,"data":{"total":2,"torrents":[{"size":1610612736,"seeding":3}]}}`), nil
		})
		v, e := Parse(t.Context(), f, []byte(`<meta name="x-csrf-token" content="fixture-csrf">Powered By TNode`), Options{})
		if e != nil || v.Schema != "TNode" || pages != 2 || v.Upload != 9007199254740993 || v.Unread != 6 || v.Seeding != 2 {
			t.Fatalf("%+v pages=%d err=%v", v, pages, e)
		}
	})
	t.Run("Mteam", func(t *testing.T) {
		pages := 0
		f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
			if r.Path == "https://api.fixture.invalid/api/member/profile" {
				return []byte(`{"data":{"id":"42","username":"fixture","role":"10","createdDate":"2020-01-02","memberStatus":{"lastBrowse":"2026-01-01"},"memberCount":{"uploaded":"9007199254740993","downloaded":"1073741824","shareRate":"2.5","bonus":"123.75"}}}`), nil
			}
			pages++
			m := r.JSON.(map[string]any)
			if m["pageNumber"] != pages || m["type"] != "SEEDING" {
				t.Fatal("pagination lost", m)
			}
			return []byte(`{"data":{"total":2,"totalPages":2,"data":[{"torrent":{"size":"1610612736","status":{"seeders":"3"}}}]}}`), nil
		})
		v, e := Parse(t.Context(), f, []byte(`M-Team`), Options{MTeamAPI: "https://api.fixture.invalid"})
		if e != nil || v.Schema != "Mteam" || pages != 2 || v.Upload != 9007199254740993 || v.Seeding != 2 || v.Level != "VIP" {
			t.Fatalf("%+v pages=%d err=%v", v, pages, e)
		}
	})
}

func TestNexusUnreadContentsAreExplicitAndDeduplicated(t *testing.T) {
	home := `<title>fixture</title><a href="userdetails.php?id=42">fixture</a><div><a href="messages.php">信息箱 (1)</a></div>`
	for _, read := range []bool{false, true} {
		t.Run(fmt.Sprint(read), func(t *testing.T) {
			reads := 0
			f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
				switch {
				case r.Path == "userdetails.php?id=42":
					return []byte(fixtureProfile + `<div><a href="messages.php">信息箱 (0)</a></div>`), nil
				case strings.Contains(r.Path, "getusertorrentlistajax"):
					return []byte(seedTable(5, 3, 4)), nil
				case strings.Contains(r.Path, "viewmailbox"):
					return []byte(`<table><tr><td><a href="messages.php?action=viewmessage&id=1">unread</a></td></tr><tr><td><img alt="Read"><a href="messages.php?action=viewmessage&id=2">read</a></td></tr></table>`), nil
				case strings.Contains(r.Path, "viewmessage"):
					if !r.Message {
						t.Fatal("read effect not declared")
					}
					reads++
					return []byte(`<h1>Fixture notice</h1><table><tr><td class="colhead">From</td></tr><tr><td>fixture</td><td>2026-11-03</td></tr><tr><td>Fixture content</td></tr></table>`), nil
				default:
					return nil, ErrResponse
				}
			})
			v, e := Parse(t.Context(), f, []byte(home), Options{ReadMessages: read})
			if e != nil {
				t.Fatal(e)
			}
			want := 0
			if read {
				want = 1
			}
			if reads != want || len(v.Messages) != want || v.Unread != 1 {
				t.Fatalf("reads=%d snapshot=%+v", reads, v)
			}
		})
	}
}

func TestIncompletePagesAndLoginNeverBecomeSnapshots(t *testing.T) {
	home := []byte(`<title>fixture</title><a href="userdetails.php?id=42">fixture</a>`)
	for _, mode := range []string{"failed-detail", "missing-traffic", "pagination-loop", "malformed-seeds", "login"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
				calls++
				if r.Path == "userdetails.php?id=42" {
					switch mode {
					case "failed-detail":
						return nil, ErrResponse
					case "missing-traffic":
						return []byte(`<p>no statistics</p>`), nil
					case "login":
						return []byte(`<input type="password">`), nil
					}
					return []byte(fixtureProfile), nil
				}
				if mode == "malformed-seeds" {
					return []byte(`<p>broken upstream</p>`), nil
				}
				return []byte(seedTable(5, 3, 4) + `<a href="?page=1">下一页</a>`), nil
			})
			if _, e := Parse(t.Context(), f, home, Options{}); e == nil {
				t.Fatal("incomplete snapshot accepted")
			}
			if calls > 6 {
				t.Fatal("pagination did not stop", calls)
			}
		})
	}
}

func TestByteParsingKeepsInt64Precision(t *testing.T) {
	for _, tc := range []struct {
		in string
		n  int64
		ok bool
	}{{"9007199254740993", 9007199254740993, true}, {"1.5 GiB", 1610612736, true}, {"2,048 MiB", 2147483648, true}, {"9223372036854775808", 0, false}, {"unknown", 0, false}} {
		n, ok := size(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("%s => %d,%v", tc.in, n, ok)
		}
	}
}
