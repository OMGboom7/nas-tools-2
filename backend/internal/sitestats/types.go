// Package sitestats implements tracker user statistics without a Python runtime.
package sitestats

import (
	"context"
	"errors"
	"net/url"
)

var (
	ErrResponse = errors.New("站点响应缺少有效统计数据")
	ErrLogin    = errors.New("未检测到已登录，请更新 Cookie")
	ErrPage     = errors.New("站点分页异常或超过上限")
	ErrOrigin   = errors.New("站点链接超出当前站点")
)

type Request struct {
	Raw     bool // JSON and binary responses must remain raw even in browser mode.
	Path    string
	Form    url.Values
	JSON    any
	Headers map[string]string
	Message bool // Reading a message can mark it as read remotely.
}
type Fetcher interface {
	Fetch(context.Context, Request) ([]byte, error)
}
type Message struct {
	Title   string `json:"title"`
	Date    string `json:"date"`
	Content string `json:"content"`
}
type Snapshot struct {
	SeedDetailsKnown           bool       `json:"-"`
	Site                       string     `json:"site"`
	URL                        string     `json:"url"`
	Schema                     string     `json:"schema"`
	UserID                     string     `json:"userId"`
	Username                   string     `json:"username"`
	Level                      string     `json:"userLevel"`
	Joined                     string     `json:"joinAt"`
	LastSeen                   string     `json:"lastSeen"`
	Updated                    string     `json:"updateAt"`
	Upload                     int64      `json:"upload"`
	Download                   int64      `json:"download"`
	Ratio                      float64    `json:"ratio"`
	Bonus                      float64    `json:"bonus"`
	Seeding                    int64      `json:"seeding"`
	Leeching                   int64      `json:"leeching"`
	SeedingSize                int64      `json:"seedingSize"`
	Unread                     int64      `json:"messageUnread"`
	Seeds                      [][2]int64 `json:"seedingInfo"`
	Messages                   []Message  `json:"messages,omitempty"`
	Favicon                    string     `json:"favicon,omitempty"`
	uploadFound, downloadFound bool
}

type Options struct {
	Site, URL    string
	ReadMessages bool
	// MTeamAPI is derived from the configured M-Team host by the caller.
	// The fetcher must scope its API key to this distinct origin.
	MTeamAPI string
}

const MaxPages = 200
const MaxSeeds = 100000

func Identify(page string) string {
	switch {
	case contains(page, "Powered by Gazelle", "DIC Music"):
		return "Gazelle"
	case contains(page, "M-Team"):
		return "Mteam"
	case contains(page, "Style by Rabbit"):
		return "NexusRabbit"
	case contains(page, "Powered by Discuz!"):
		return "Discuz"
	case contains(page, "unit3d.js"):
		return "Unit3d"
	case contains(page, "Nexus Project"):
		return "NexusProject"
	case contains(page, "Small Horse"):
		return "SmallHorse"
	case contains(page, "IPTorrents"):
		return "Ipt"
	case contains(page, "TorrentLeech"):
		return "TorrentLeech"
	case contains(page, "Powered by FileList"):
		return "FileList"
	case contains(page, "Powered By TNode"):
		return "TNode"
	default:
		return "NexusPhp"
	}
}
