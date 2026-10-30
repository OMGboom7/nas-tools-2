package opensubtitles

import (
	"archive/zip"
	"bytes"
	"io"
	"path"
	"strings"
)

const MaxDownload = 32 << 20
const MaxSubtitle = 16 << 20

type File struct {
	Extension string
	Data      []byte
}

func Extension(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == ".srt" || ext == ".ass" || ext == ".ssa" {
		return ext
	}
	return ""
}

// ZIP members are never extracted as paths. Names only select one of the three
// original subtitle extensions; output paths come from the proven video.
func Archive(raw []byte) ([]File, error) {
	if len(raw) == 0 || len(raw) > MaxDownload {
		return nil, ErrLimit
	}
	z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		return nil, ErrResponse
	}
	if len(z.File) > 2048 {
		return nil, ErrLimit
	}
	out := []File{}
	var total uint64
	for _, f := range z.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if name == "" || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "/") || strings.Contains(strings.Split(name, "/")[0], ":") {
			return nil, ErrResponse
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." || part == "." {
				return nil, ErrResponse
			}
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() || f.Flags&1 != 0 {
			return nil, ErrResponse
		}
		if f.UncompressedSize64 > 64<<20 || total+f.UncompressedSize64 > 64<<20 {
			return nil, ErrLimit
		}
		total += f.UncompressedSize64
		ext := Extension(name)
		if ext == "" {
			continue
		}
		if f.UncompressedSize64 == 0 || f.UncompressedSize64 > MaxSubtitle {
			return nil, ErrLimit
		}
		rd, e := f.Open()
		if e != nil {
			return nil, ErrResponse
		}
		data, e := io.ReadAll(io.LimitReader(rd, MaxSubtitle+1))
		closeErr := rd.Close()
		if e != nil || closeErr != nil || len(data) > MaxSubtitle || uint64(len(data)) != f.UncompressedSize64 || !SubtitleBytes(data) {
			return nil, ErrResponse
		}
		out = append(out, File{Extension: ext, Data: data})
	}
	return out, nil
}
func SubtitleBytes(data []byte) bool {
	if len(data) == 0 || len(data) > MaxSubtitle {
		return false
	}
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	head = bytes.TrimSpace(bytes.TrimPrefix(head, []byte{0xef, 0xbb, 0xbf}))
	lower := bytes.ToLower(head)
	return !bytes.HasPrefix(lower, []byte("<!doctype html")) && !bytes.HasPrefix(lower, []byte("<html")) && !bytes.Contains(lower, []byte("cf-chl-")) && !bytes.HasPrefix(lower, []byte("{\"error"))
}
