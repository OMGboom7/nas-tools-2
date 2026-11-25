package organization

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type DiskRoot struct{ Path, Identity, Configured string }
type DiskFile struct {
	Path string
	Node LibraryNode
}
type DiskPair struct {
	Root            DiskRoot
	Keep, Duplicate DiskFile
}
type DiskScan struct {
	Files, ExistingLinks, Failed int
	Pairs                        []DiskPair
}
type DiskCacheRow struct {
	Path     string `json:"filePath"`
	Size     int64  `json:"fileSize"`
	Modified string `json:"fileModifyTime"`
	SHA1     string `json:"fileSha1"`
	Stamp    string `json:"goStamp,omitempty"`
}
type DiskCache struct {
	Rows   []DiskCacheRow  `json:"file_info"`
	Inodes json.RawMessage `json:"inode_info"`
}

// Freeze the configured mount once so a missing or remounted directory cannot
// become a new scan target after the one-shot request was saved.
func FreezeDiskRoot(path string) (DiskRoot, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return DiskRoot{}, ErrPath
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		return DiskRoot{}, err
	}
	st, err := os.Stat(actual)
	if err != nil || !st.IsDir() || actual == "/" || Identity(st) == "" {
		return DiskRoot{}, ErrPath
	}
	return DiskRoot{Path: actual, Identity: Identity(st), Configured: path}, nil
}

func diskStageName(job string, index int) string {
	return ".nastool-dedup-" + Digest([]any{job, index})
}

func diskDevice(n LibraryNode) string {
	dev, _, _ := strings.Cut(n.Identity, ":")
	return dev
}

func DiskStagePath(pair DiskPair, job string, index int) string {
	return filepath.Join(pair.Root.Path, filepath.Dir(pair.Duplicate.Path), diskStageName(job, index))
}
