//go:build linux || darwin

package scraper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var ErrThumbnail = errors.New("video thumbnail unavailable; no media was changed")

type thumbnailBuffer struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *thumbnailBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 8<<20 {
		b.cancel()
		return 0, ErrThumbnail
	}
	return b.buffer.Write(p)
}
func (b *thumbnailBuffer) Bytes() []byte { return b.buffer.Bytes() }

// RenderThumbnail receives only an already pinned readonly regular-file
// descriptor, not a path or URL. Nothing from an API request becomes an option.
func RenderThumbnail(ctx context.Context, source *os.File) ([]byte, error) {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil || !filepath.IsAbs(binary) {
		return nil, ErrThumbnail
	}
	return renderThumbnail(ctx, source, func(ctx context.Context, args ...string) *exec.Cmd { return exec.CommandContext(ctx, binary, args...) }, 30*time.Second)
}
func renderThumbnail(ctx context.Context, source *os.File, newCommand func(context.Context, ...string) *exec.Cmd, timeout time.Duration) ([]byte, error) {
	if source == nil || ctx.Err() != nil || timeout <= 0 {
		return nil, ErrThumbnail
	}
	info, err := source.Stat()
	flags, e := unix.FcntlInt(source.Fd(), unix.F_GETFL, 0)
	if err != nil || e != nil || !info.Mode().IsRegular() || flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, ErrThumbnail
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, ErrThumbnail
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// fd supports seeking; excluding file/http/concat/etc. also prevents a
	// container or playlist from opening external media or network resources.
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-max_alloc", "67108864", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2,mpegts,avi,mpeg,mpegvideo,flv,asf,ogg", "-ss", "181", "-fd", "3", "-i", "fd:", "-map", "0:v:0", "-frames:v", "1", "-an", "-sn", "-dn", "-vf", "scale=1280:720:force_original_aspect_ratio=decrease", "-threads", "1", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1"}
	cmd := newCommand(call, args...)
	cmd.ExtraFiles = []*os.File{source}
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	out := &thumbnailBuffer{cancel: cancel}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil || call.Err() != nil || ValidateImage(out.Bytes(), ".jpg") != nil {
		return nil, ErrThumbnail
	}
	return out.Bytes(), nil
}
