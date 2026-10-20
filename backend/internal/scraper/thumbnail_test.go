//go:build linux || darwin

package scraper

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestThumbnailChildProcess(t *testing.T) {
	i := slices.Index(os.Args, "thumbnail-child")
	if i < 0 {
		return
	}
	mode := os.Args[i+1]
	args := os.Args[i+2:]
	if !slices.Contains(args, "fd:") || !slices.Contains(args, "pipe:1") || !slices.Contains(args, "fd,pipe") || slices.Contains(args, "-y") || os.Getenv("FFREPORT") != "" {
		os.Exit(5)
	}
	f := os.NewFile(3, "approved input")
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "safe media" {
		os.Exit(6)
	}
	switch mode {
	case "hang":
		time.Sleep(20 * time.Second)
	case "oversize":
		os.Stdout.Write(bytes.Repeat([]byte("x"), (8<<20)+1))
	case "bad":
		os.Stdout.WriteString("not an image")
	case "fail":
		os.Stderr.WriteString("private source or credential")
		os.Exit(7)
	default:
		jpeg.Encode(os.Stdout, image.NewRGBA(image.Rect(0, 0, 16, 9)), nil)
	}
	os.Exit(0)
}
func TestThumbnailProcessIsBoundedReadonlyAndNeverUsesAShellOrPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), `Movie; touch should-not-exist.mkv`)
	if err := os.WriteFile(path, []byte("safe media"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("FFREPORT", "private-report-file")
	for _, mode := range []string{"ok", "bad", "fail", "oversize", "hang"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			timeout := 5 * time.Second
			if mode == "hang" {
				timeout = 80 * time.Millisecond
			}
			data, err := renderThumbnail(t.Context(), f, func(ctx context.Context, args ...string) *exec.Cmd {
				for _, arg := range args {
					if strings.Contains(arg, path) || strings.Contains(arg, "should-not-exist") {
						t.Fatal("user path became command argument")
					}
				}
				argv := append([]string{"-test.run=^TestThumbnailChildProcess$", "--", "thumbnail-child", mode}, args...)
				return exec.CommandContext(ctx, os.Args[0], argv...)
			}, timeout)
			if mode == "ok" {
				if err != nil || ValidateImage(data, ".jpg") != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal(mode, err)
			}
			if mode == "hang" && time.Since(start) > 3*time.Second {
				t.Fatal("cancel did not join process group")
			}
		})
	}
	b, _ := os.ReadFile(path)
	if string(b) != "safe media" {
		t.Fatal("decoder wrote media")
	}
	rw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := renderThumbnail(t.Context(), rw, func(context.Context, ...string) *exec.Cmd { t.Fatal("writable descriptor was executed"); return nil }, time.Second); err == nil {
		t.Fatal("writable descriptor accepted")
	}
}
func TestRealFFmpegProduces181SecondJPEGFromPinnedSyntheticVideo(t *testing.T) {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable; bounded helper-process tests still run")
	}
	path := filepath.Join(t.TempDir(), "Synthetic.S01E01.mkv")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "183", "-c:v", "ffv1", "-threads", "1", path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("synthetic video generation failed", err, string(b))
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, _ := f.Stat()
	data, err := RenderThumbnail(t.Context(), f)
	if err != nil || ValidateImage(data, ".jpg") != nil {
		t.Fatal("real pinned descriptor decoding failed", err)
	}
	after, _ := f.Stat()
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatal("decoder changed source")
	}
}
