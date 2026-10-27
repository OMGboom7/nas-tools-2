package torrentmeta

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var ErrMetadataUnavailable = errors.New("bounded metadata-only resolver unavailable")

// ResolveMagnet replaces the old Python/libtorrent bridge. aria2 is restricted
// to metadata only; neither a shell nor a media/download directory is supplied.
// Private tracker URLs use stdin, not process arguments or captured logs.
func ResolveMagnet(ctx context.Context, magnet, hash string) ([]byte, error) {
	if len(hash) != 40 || len(magnet) > 4096 || !strings.HasPrefix(magnet, "magnet:?") || strings.ContainsAny(magnet, "\r\n\x00") {
		return nil, ErrMetadataUnavailable
	}
	for _, ch := range hash {
		if ch < '0' || ch > '9' {
			if ch < 'a' || ch > 'f' {
				return nil, ErrMetadataUnavailable
			}
		}
	}
	binary := os.Getenv("NASTOOL_ARIA2_BINARY")
	if binary == "" {
		binary = "aria2c"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, ErrMetadataUnavailable
	}
	dir, err := os.MkdirTemp("", "nastool-torrent-metadata-")
	if err != nil {
		return nil, ErrMetadataUnavailable
	}
	// This private directory is created here and never accepts a user path.
	defer os.RemoveAll(dir)
	call, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, resolved, "--no-conf=true", "--enable-rpc=false", "--bt-metadata-only=true", "--bt-save-metadata=true", "--follow-torrent=false", "--seed-time=0", "--max-concurrent-downloads=1", "--file-allocation=none", "--auto-save-interval=0", "--summary-interval=0", "--console-log-level=error", "--log-level=error", "--download-result=hide", "--enable-color=false", "--dht-file-path="+filepath.Join(dir, "dht.dat"), "--dht-file-path6="+filepath.Join(dir, "dht6.dat"), "--dir="+dir, "--input-file=-")
	if os.Getenv("NASTOOL_METADATA_DISABLE_DHT") == "true" {
		cmd.Args = append(cmd.Args, "--enable-dht=false", "--enable-dht6=false")
	}
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(magnet + "\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Dir = dir
	if cmd.Run() != nil {
		return nil, ErrMetadataUnavailable
	}
	name := filepath.Join(dir, hash+".torrent")
	stat, err := os.Lstat(name)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 8<<20 {
		return nil, ErrMetadataUnavailable
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, ErrMetadataUnavailable
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil || len(raw) > 8<<20 {
		return nil, ErrMetadataUnavailable
	}
	meta, _, err := Manifest(raw)
	if err != nil || meta.InfoHash != hash {
		return nil, ErrMetadataUnavailable
	}
	return raw, nil
}
