package remotestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func videoDigest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func TestMinioVerifiedRangesAreExactSeekableBoundedAndContentPinned(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := make([]byte, VerifiedBlockSize+513)
	for i := range data {
		data[i] = byte(i % 251)
	}
	s.Put("bucket/library/中文;$(not-a-command).mp4", data)
	var revoked atomic.Bool
	current := func(ctx context.Context) error {
		if revoked.Load() {
			return ErrChanged
		}
		return ctx.Err()
	}
	o, err := c.Pin(t.Context(), "/bucket/library", "中文;$(not-a-command).mp4", int64(len(data)), videoDigest(data), current)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	for _, spec := range []struct {
		offset int64
		count  int
	}{{0, 17}, {VerifiedBlockSize - 7, 30}, {VerifiedBlockSize + 500, 30}} {
		out := make([]byte, spec.count)
		n, err := o.ReadAt(t.Context(), out, spec.offset)
		want := min(spec.count, len(data)-int(spec.offset))
		if n != want || !bytes.Equal(out[:n], data[spec.offset:int(spec.offset)+n]) || want == spec.count && err != nil || want < spec.count && !errors.Is(err, io.EOF) {
			t.Fatal(spec, n, err)
		}
	}
	var out bytes.Buffer
	if err := c.ReadRange(t.Context(), "/bucket/library/中文;$(not-a-command).mp4", int64(len(data)), VerifiedBlockSize+500, 13, &out); err != nil || !bytes.Equal(out.Bytes(), data[len(data)-13:]) {
		t.Fatal(err)
	}
	for _, spec := range []struct{ size, offset, count int64 }{{0, 0, 1}, {10, -1, 1}, {10, 10, 1}, {10, 9, 2}, {10, 0, 1 << 62}, {1 << 62, 0, 1}, {10, 0, 0}} {
		before := len(s.Requests())
		if c.ReadRange(t.Context(), "/bucket/library/中文;$(not-a-command).mp4", spec.size, spec.offset, spec.count, io.Discard) == nil || len(s.Requests()) != before {
			t.Fatal("invalid range reached service", spec)
		}
	}
	for _, mode := range []string{"ignore", "bounds", "short", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			object, err := c.Pin(t.Context(), "/bucket/library", "中文;$(not-a-command).mp4", int64(len(data)), videoDigest(data), current)
			if err != nil {
				t.Fatal(err)
			}
			defer object.Close()
			s.RangeFault(mode)
			defer s.RangeFault("")
			buf := bytes.Repeat([]byte{0x7f}, 30)
			if n, err := object.ReadAt(t.Context(), buf, VerifiedBlockSize+2); err == nil || n != 0 || !bytes.Equal(buf, bytes.Repeat([]byte{0x7f}, 30)) {
				t.Fatal("unverified response exposed", mode, n, err)
			}
		})
	}
	before := len(s.Requests())
	revoked.Store(true)
	if _, err := o.ReadAt(t.Context(), make([]byte, 3), 0); err == nil || len(s.Requests()) != before {
		t.Fatal("revoked capability reached service", err)
	}
	revoked.Store(false)
	o.Close()
	if _, err := o.ReadAt(t.Context(), make([]byte, 3), 0); err == nil || len(s.Requests()) != before {
		t.Fatal("closed capability escaped callback", err)
	}
	for _, r := range s.Requests() {
		if r.Method != "HEAD" && r.Method != "GET" {
			t.Fatal("reader wrote to storage")
		}
	}
}

func TestVerifiedPinRejectsWrongWholeDigestAndNoConsentOrOversizeWithoutNetwork(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s.Put("bucket/library/video.mkv", []byte("original video"))
	if _, err := c.Pin(t.Context(), "/bucket/library", "video.mkv", 14, strings.Repeat("0", 64), func(ctx context.Context) error { return ctx.Err() }); err == nil {
		t.Fatal("wrong full digest accepted")
	}
	for _, args := range []struct {
		name  string
		size  int64
		guard func(context.Context) error
	}{{"video.mkv", MaxVerifiedMediaSize + 1, func(ctx context.Context) error { return nil }}, {"../video.mkv", 14, func(ctx context.Context) error { return nil }}, {"video.mkv", 14, nil}} {
		before := len(s.Requests())
		if _, err := c.Pin(t.Context(), "/bucket/library", args.name, args.size, videoDigest([]byte("original video")), args.guard); err == nil || len(s.Requests()) != before {
			t.Fatal("unbounded/unapproved pin reached service", err)
		}
	}
}

func TestVerifiedRangeCancellationAccountRevocationAndBudgetDoNotReadOrReplay(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := []byte("original video")
	s.Put("bucket/library/video.mkv", data)
	pin := func() *VerifiedObject {
		o, err := c.Pin(t.Context(), "/bucket/library", "video.mkv", int64(len(data)), videoDigest(data), func(ctx context.Context) error { return ctx.Err() })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(o.Close)
		return o
	}
	for _, mode := range []string{"cancel", "close", "budget", "account"} {
		t.Run(mode, func(t *testing.T) {
			o := pin()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "cancel":
				cancel()
			case "close":
				o.Close()
			case "budget":
				o.readBytes = maxVerifiedReadBytes
			case "account":
				raw, _ := os.ReadFile(f.ConfigFile)
				if err := os.WriteFile(f.ConfigFile, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := o.ReadAt(ctx, make([]byte, 3), 0); n != 0 || err == nil {
				t.Fatal(mode, n, err)
			}
		})
	}
}

func TestRealRcloneVerifiedRangesSeekWithoutLocalVideoCopyOrAmbientCredentials(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit isolated Rclone binary required")
	}
	root := t.TempDir()
	data := make([]byte, VerifiedBlockSize+113)
	for i := range data {
		data[i] = byte(i % 239)
	}
	name := "Show;$(never-executed).S01E02.mp4"
	if err := os.Mkdir(filepath.Join(root, "library"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "library", name)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("[NASTOOL]\ntype = alias\nremote = "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := (Factory{ConfigFile: config, Binary: binary, Environment: []string{}}).Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	o, err := c.Pin(t.Context(), "/library", name, int64(len(data)), videoDigest(data), func(ctx context.Context) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	buf := make([]byte, 37)
	if n, err := o.ReadAt(t.Context(), buf, VerifiedBlockSize-17); err != nil || n != len(buf) || !bytes.Equal(buf, data[VerifiedBlockSize-17:VerifiedBlockSize+20]) {
		t.Fatal(n, err)
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".conf") {
			t.Fatal("reader retained a second video", e.Name())
		}
	}
	info, _ := os.Stat(file)
	data[VerifiedBlockSize+2] ^= 1
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if n, err := o.ReadAt(t.Context(), buf, VerifiedBlockSize+2); err == nil || n != 0 {
		t.Fatal("same-size/time changed bytes exposed", n, err)
	}
}
