package autobackup

import "net/url"

type Target struct {
	Path     string `json:"path"`
	Identity string `json:"-"`
}

type Archive struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"-"`
}

type Sources struct {
	Config, Category, FallbackCategory, Database string
}

func sqliteDSN(path, mode string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode + "&_pragma=busy_timeout(5000)"}).String()
}
