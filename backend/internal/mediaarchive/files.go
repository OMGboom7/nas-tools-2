// Package mediaarchive manages the existing Markdown archive files.
package mediaarchive

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Plugin = "MediaLibraryArchive"
const PageSize = 200

var ErrChanged = errors.New("archive file or directory changed")
var filename = regexp.MustCompile(`^归档_([0-9]{14})\.md$`)

type File struct {
	Name, Time, Proof string
	Size              int64
}

func timestamp(name string) string {
	match := filename.FindStringSubmatch(name)
	if match == nil {
		return ""
	}
	t, err := time.Parse("20060102150405", match[1])
	if err != nil || t.Year() == 0 {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func ValidName(name string) bool { return timestamp(name) != "" }

// The original Docker plugin always uses the configuration directory.
// Custom paths on a standalone host follow the service working directory.
func Open(configFile, custom string, container bool) (*os.File, error) {
	return directory(configFile, custom, container, false)
}

func directory(configFile, custom string, container, create bool) (*os.File, error) {
	if configFile == "" {
		return nil, ErrChanged
	}
	if container || custom == "" {
		return openDirectory(filepath.Dir(configFile), []string{"archive_file", "media_library"}, create)
	}
	if strings.ContainsRune(custom, 0) {
		return nil, ErrChanged
	}
	return openDirectory(custom, nil, create)
}

type Result struct {
	Name      string `json:"name,omitempty"`
	Libraries int    `json:"libraries"`
	Removed   int    `json:"removed"`
}
