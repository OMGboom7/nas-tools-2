//go:build linux || darwin

package scraper

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type thumbnailMemorySource struct {
	data  []byte
	reads atomic.Int32
	far   atomic.Bool
	bad   atomic.Bool
}

func (s *thumbnailMemorySource) Size() int64 { return int64(len(s.data)) }
func (s *thumbnailMemorySource) Current(ctx context.Context) error {
	if s.bad.Load() {
		return ErrThumbnail
	}
	return ctx.Err()
}
func (s *thumbnailMemorySource) ReadAt(ctx context.Context, p []byte, offset int64) (int, error) {
	if err := s.Current(ctx); err != nil {
		return 0, err
	}
	s.reads.Add(1)
	if offset > 4<<20 {
		s.far.Store(true)
	}
	if offset < 0 || offset >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[offset:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestThumbnailRangeParsesExactSingleRangesAndRejectsOverflowAndMultiparts(t *testing.T) {
	for _, spec := range []struct {
		input      string
		start, end int64
		partial    bool
		bad        bool
	}{
		{"", 0, 99, false, false}, {"bytes=0-", 0, 99, true, false}, {"bytes=20-29", 20, 29, true, false}, {"bytes=-9", 91, 99, true, false}, {"bytes=-200", 0, 99, true, false}, {"bytes=90-200", 90, 99, true, false},
		{"bytes=100-", 0, 0, false, true}, {"bytes=20-19", 0, 0, false, true}, {"bytes=-0", 0, 0, false, true}, {"bytes=0-1,2-3", 0, 0, false, true}, {"bytes=+1-2", 0, 0, false, true}, {"bytes=0-9223372036854775808", 0, 0, false, true}, {"bytes= 1-2", 0, 0, false, true}, {"bytes=--1", 0, 0, false, true}, {"Bytes=0-1", 0, 0, false, true},
	} {
		a, b, partial, err := thumbnailRange(spec.input, 100)
		if spec.bad {
			if err == nil {
				t.Fatal(spec.input, "accepted")
			}
		} else if err != nil || a != spec.start || b != spec.end || partial != spec.partial {
			t.Fatal(spec, a, b, partial, err)
		}
	}
}

func TestThumbnailBridgeOnlyServesTheExactCapabilityWithCorrectHeadersAndNoWrites(t *testing.T) {
	s := &thumbnailMemorySource{data: []byte("0123456789")}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &thumbnailBridge{source: s, host: "127.0.0.1:1234", route: "/private-capability", ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	request := func(method, target, host, bounds string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = host
		if bounds != "" {
			r.Header.Set("Range", bounds)
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		return w
	}
	for _, spec := range []struct{ method, target, host string }{{"POST", b.route, b.host}, {"GET", "/other", b.host}, {"GET", b.route + "?token=anything", b.host}, {"GET", b.route, "evil.test"}, {"GET", "http://" + b.host + b.route, b.host}, {"GET", "/%70rivate-capability", b.host}} {
		w := request(spec.method, spec.target, spec.host, "")
		if w.Code != 404 || s.reads.Load() != 0 {
			t.Fatal("foreign request read source", spec, w.Code)
		}
	}
	for _, spec := range []struct {
		bounds, want, content string
		status                int
	}{{"", "0123456789", "", 200}, {"bytes=2-5", "2345", "bytes 2-5/10", 206}, {"bytes=-3", "789", "bytes 7-9/10", 206}, {"bytes=8-", "89", "bytes 8-9/10", 206}} {
		w := request("GET", b.route, b.host, spec.bounds)
		if w.Code != spec.status || w.Body.String() != spec.want || w.Header().Get("Content-Range") != spec.content || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(spec, w.Code, w.Body.String(), w.Header())
		}
	}
	reads := s.reads.Load()
	w := request("HEAD", b.route, b.host, "bytes=4-6")
	if w.Code != 206 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "3" || s.reads.Load() != reads {
		t.Fatal("HEAD read body", w.Code)
	}
	w = request("GET", b.route, b.host, "bytes=100-")
	if w.Code != 416 || w.Header().Get("Content-Range") != "bytes */10" || s.reads.Load() != reads {
		t.Fatal("unsatisfiable range read body", w.Code)
	}
	for _, mode := range []string{"bytes", "requests", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			call, stop := context.WithCancel(t.Context())
			defer stop()
			bridge := &thumbnailBridge{source: s, host: b.host, route: b.route, ctx: call, cancel: stop, gate: make(chan struct{}, 1)}
			switch mode {
			case "bytes":
				bridge.bytes = thumbnailReadBudget
			case "requests":
				bridge.requests = thumbnailRequestBudget
			case "revoked":
				s.bad.Store(true)
				defer s.bad.Store(false)
			}
			r := httptest.NewRequest("GET", bridge.route, nil)
			r.Host = bridge.host
			w := httptest.NewRecorder()
			bridge.ServeHTTP(w, r)
			if !bridge.failed.Load() || call.Err() == nil || s.reads.Load() != reads || w.Body.Len() != 0 {
				t.Fatal("budget/revocation did not invalidate before read", mode)
			}
		})
	}
}

func TestRemoteThumbnailChildProcess(t *testing.T) {
	i := slices.Index(os.Args, "remote-thumbnail-child")
	if i < 0 {
		return
	}
	mode, args := os.Args[i+1], os.Args[i+2:]
	input := slices.Index(args, "-i")
	if input < 0 || input+1 >= len(args) || !slices.Contains(args, "http,tcp,pipe") || !slices.Contains(args, "pipe:1") || slices.Contains(args, "-fd") || slices.Contains(args, "-y") || os.Getenv("FFREPORT") != "" || os.Getenv("HTTP_PROXY") != "" {
		os.Exit(5)
	}
	u, err := url.Parse(args[input+1])
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || len(u.Path) != 65 {
		os.Exit(6)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	for _, spec := range []struct{ rangeValue, want string }{{"bytes=5-9", "media"}, {"bytes=0-3", "safe"}} {
		r, _ := http.NewRequest("GET", u.String(), nil)
		r.Header.Set("Range", spec.rangeValue)
		response, err := client.Do(r)
		if err != nil {
			os.Exit(7)
		}
		data, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil || response.StatusCode != 206 || string(data) != spec.want {
			os.Exit(8)
		}
	}
	switch mode {
	case "hang":
		time.Sleep(20 * time.Second)
	case "bad":
		os.Stdout.WriteString("bad")
	case "fail":
		os.Stderr.WriteString("private cloud account and token")
		os.Exit(9)
	case "oversize":
		os.Stdout.Write(bytes.Repeat([]byte("x"), (8<<20)+1))
	default:
		jpeg.Encode(os.Stdout, image.NewRGBA(image.Rect(0, 0, 16, 9)), nil)
	}
	os.Exit(0)
}

func TestRemoteThumbnailProcessUsesOnlyLoopbackCapabilityAndAlwaysJoinsItsBridge(t *testing.T) {
	t.Setenv("FFREPORT", "private-report")
	t.Setenv("HTTP_PROXY", "http://do-not-use.invalid")
	for _, mode := range []string{"ok", "bad", "fail", "oversize", "hang"} {
		t.Run(mode, func(t *testing.T) {
			s := &thumbnailMemorySource{data: []byte("safe media")}
			var address string
			timeout := 5 * time.Second
			if mode == "hang" {
				timeout = 300 * time.Millisecond
			}
			start := time.Now()
			data, err := renderRemoteThumbnail(t.Context(), s, func(ctx context.Context, args ...string) *exec.Cmd {
				address = args[slices.Index(args, "-i")+1]
				argv := append([]string{"-test.run=^TestRemoteThumbnailChildProcess$", "--", "remote-thumbnail-child", mode}, args...)
				return exec.CommandContext(ctx, os.Args[0], argv...)
			}, timeout)
			if mode == "ok" {
				if err != nil || ValidateImage(data, ".jpg") != nil || s.reads.Load() != 3 {
					t.Fatal(err, s.reads.Load())
				}
			} else if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal(mode, err)
			}
			if mode == "hang" && time.Since(start) > 3*time.Second {
				t.Fatal("decoder group or bridge did not join")
			}
			client := http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
			response, e := client.Get(address)
			if e == nil {
				response.Body.Close()
				t.Fatal("capability listener survived decoder")
			}
		})
	}
}

type blockedThumbnailSource struct {
	started chan struct{}
	once    sync.Once
	reads   atomic.Int32
}

func (s *blockedThumbnailSource) Size() int64                       { return 10 }
func (s *blockedThumbnailSource) Current(ctx context.Context) error { return ctx.Err() }
func (s *blockedThumbnailSource) ReadAt(ctx context.Context, p []byte, _ int64) (int, error) {
	if s.reads.Add(1) == 1 {
		return copy(p, []byte("safe media")), nil
	}
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestRemoteThumbnailCancellationJoinsBlockedRangeAndChild(t *testing.T) {
	s := &blockedThumbnailSource{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := renderRemoteThumbnail(ctx, s, func(ctx context.Context, args ...string) *exec.Cmd {
			argv := append([]string{"-test.run=^TestRemoteThumbnailChildProcess$", "--", "remote-thumbnail-child", "ok"}, args...)
			return exec.CommandContext(ctx, os.Args[0], argv...)
		}, 5*time.Second)
		done <- err
	}()
	select {
	case <-s.started:
	case <-time.After(3 * time.Second):
		t.Fatal("range did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled reader returned success")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation did not join range/child/server")
	}
}

func TestRealFFmpegRemoteMKVAndEndMoovMP4UseSeekableSingleObject(t *testing.T) {
	binaryPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, extension := range []string{".mkv", ".mp4"} {
		t.Run(extension, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "synthetic"+extension)
			codec := "ffv1"
			if extension == ".mp4" {
				codec = "mpeg4"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=1", "-t", "183", "-c:v", codec, "-threads", "1", name)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatal("synthetic video failed", err, string(b))
			}
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if extension == ".mp4" {
				moov := bytes.Index(data, []byte("moov")) - 4
				mdat := bytes.Index(data, []byte("mdat")) - 4
				if moov <= mdat || moov < 0 {
					t.Fatal("fixture is not end-moov MP4")
				}
				padding := make([]byte, 8<<20)
				binary.BigEndian.PutUint32(padding, uint32(len(padding)))
				copy(padding[4:], "free")
				data = append(append(append([]byte{}, data[:moov]...), padding...), data[moov:]...)
			}
			s := &thumbnailMemorySource{data: data}
			diagnostic := filepath.Join(t.TempDir(), "synthetic-ffmpeg-diagnostic.txt")
			image, err := renderRemoteThumbnail(t.Context(), s, func(ctx context.Context, args ...string) *exec.Cmd {
				argv := append([]string{"-test.run=^TestRemoteFFmpegDiagnosticChild$", "--", "remote-ffmpeg-diagnostic", binaryPath, diagnostic}, args...)
				return exec.CommandContext(ctx, os.Args[0], argv...)
			}, 90*time.Second)
			if err != nil || ValidateImage(image, ".jpg") != nil || s.reads.Load() == 0 {
				message, _ := os.ReadFile(diagnostic)
				t.Fatal("synthetic remote video failed", err, s.reads.Load(), string(message))
			}
			if extension == ".mp4" && !s.far.Load() {
				t.Fatal("MP4 did not seek to remote tail")
			}
		})
	}
}

// Diagnostics here contain only generated media and a disposable loopback
// capability. Production always discards decoder stderr and never runs a shell.
func TestRemoteFFmpegDiagnosticChild(t *testing.T) {
	i := slices.Index(os.Args, "remote-ffmpeg-diagnostic")
	if i < 0 {
		return
	}
	file, err := os.Create(os.Args[i+2])
	if err != nil {
		os.Exit(11)
	}
	cmd := exec.Command(os.Args[i+1], os.Args[i+3:]...)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Stdout, cmd.Stderr = os.Stdout, file
	err = cmd.Run()
	file.Close()
	if err != nil {
		os.Exit(12)
	}
	os.Exit(0)
}

func TestRealRemoteDecoderRejectsPlaylistsWithoutReadingReferencedResources(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	var hits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.Write([]byte("must not be opened")) }))
	defer trap.Close()
	for _, data := range []string{"#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\n" + trap.URL + "/segment.ts\n#EXT-X-ENDLIST\n", "ffconcat version 1.0\nfile '" + trap.URL + "/external.mp4'\n"} {
		if _, err := RenderRemoteThumbnail(t.Context(), &thumbnailMemorySource{data: []byte(data)}); err == nil {
			t.Fatal("playlist accepted")
		}
	}
	if hits.Load() != 0 {
		t.Fatal("decoder opened media-selected network resource", hits.Load())
	}
}
