package downloadhistory

import (
	"regexp"
	"strings"
)

type Notice struct {
	Title, Stars, Source, Setting, Downloader, User, Site, Quality, Size, Torrent, Seeders, Promotion, Description string
	HR, TaskSite                                                                                                   bool
}

var htmlTag = regexp.MustCompile("(?s)<[^>]+>")

func (n Notice) Render() (title, message string) {
	lines := []string{n.Stars, "来自：" + n.Source}
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, label+"："+value)
		}
	}
	add("下载设置", n.Setting)
	add("下载器", n.Downloader)
	add("用户", n.User)
	if n.TaskSite {
		add("任务", n.Site)
	} else {
		add("站点", n.Site)
	}
	add("质量", n.Quality)
	add("大小", n.Size)
	add("种子", n.Torrent)
	add("做种数", n.Seeders)
	lines = append(lines, "促销："+n.Promotion)
	if n.HR {
		lines = append(lines, "Hit&Run：是")
	}
	if n.Description != "" {
		lines = append(lines, "描述："+htmlTag.ReplaceAllString(n.Description, ""))
	}
	return n.Title + " 开始下载", strings.Join(lines, "\n")
}
