//go:build linux || darwin

package scraper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const thumbnailReadBlock = 4 << 20
const thumbnailReadBudget int64 = 512 << 20
const thumbnailRequestBudget = 256

type thumbnailBridge struct {
	source      ThumbnailSource
	host, route string
	ctx         context.Context
	cancel      context.CancelFunc
	gate        chan struct{}
	failed      atomic.Bool
	budgetMu    sync.Mutex
	requests    int
	bytes       int64
}

func RenderRemoteThumbnail(ctx context.Context, source ThumbnailSource) ([]byte, error) {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil || !filepath.IsAbs(binary) {
		return nil, ErrThumbnail
	}
	return renderRemoteThumbnail(ctx, source, func(ctx context.Context, args ...string) *exec.Cmd { return exec.CommandContext(ctx, binary, args...) }, 90*time.Second)
}

func renderRemoteThumbnail(ctx context.Context, source ThumbnailSource, newCommand func(context.Context, ...string) *exec.Cmd, timeout time.Duration) ([]byte, error) {
	if source == nil || source.Size() < 1 || source.Size() > 1<<40 || ctx.Err() != nil || timeout <= 0 || source.Current(ctx) != nil {
		return nil, ErrThumbnail
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Private demuxer options are rejected on unrelated formats. Inspect only
	// content-verified signature bytes, and apply MOV's explicit dref policy
	// when appropriate (its default is disabled as well).
	header := make([]byte, min(int64(12), source.Size()))
	if n, err := source.ReadAt(call, header, 0); err != nil || n != len(header) {
		return nil, ErrThumbnail
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, ErrThumbnail
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		listener.Close()
		return nil, ErrThumbnail
	}
	// FFmpeg opens a new seek request before closing the previous stream. Two
	// bounded streams must be possible; a gate held by the old socket's blocked
	// Write would otherwise prevent MP4 from reaching a trailing moov atom.
	b := &thumbnailBridge{source: source, host: listener.Addr().String(), route: "/" + hex.EncodeToString(token[:]), ctx: call, cancel: cancel, gate: make(chan struct{}, 2)}
	var connections sync.WaitGroup
	var live atomic.Int32
	server := &http.Server{
		Handler: b, BaseContext: func(net.Listener) context.Context { return call },
		ReadHeaderTimeout: 2 * time.Second, IdleTimeout: time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: timeout,
		MaxHeaderBytes: 4096, DisableGeneralOptionsHandler: true,
		ErrorLog: log.New(io.Discard, "", 0),
		ConnState: func(c net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				connections.Add(1)
				if live.Add(1) > 4 {
					_ = c.Close()
				}
			case http.StateClosed, http.StateHijacked:
				live.Add(-1)
				connections.Done()
			}
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := context.AfterFunc(call, func() { _ = server.Close() })
	defer func() {
		cancel()
		stop()
		_ = server.Close()
		<-done
		connections.Wait()
	}()
	// Seeking happens through this one ephemeral loopback object. No account,
	// remote path or presigned URL is given to FFmpeg. Redirects are not served;
	// MOV external data references are explicitly disabled. Local files and
	// playlist protocols/demuxers remain excluded, and no proxy/env is inherited.
	input := []string{"-rw_timeout", "10000000", "-http_proxy", "", "-reconnect", "0", "-multiple_requests", "0", "-seekable", "1"}
	if len(header) >= 8 {
		switch string(header[4:8]) {
		case "ftyp", "moov", "mdat", "wide", "free", "skip", "uuid", "pnot":
			input = append(input, "-enable_drefs", "0", "-use_absolute_path", "0")
		}
	}
	input = append(input, "-i", "http://"+b.host+b.route)
	data, err := runThumbnail(call, nil, "http,tcp,pipe", input, newCommand, timeout)
	if err != nil || call.Err() != nil || b.failed.Load() || source.Current(call) != nil {
		return nil, ErrThumbnail
	}
	return data, nil
}

// One RFC 7233 byte range only. A suffix, an open end and a bounded end all
// preserve the object's real size; multipart/overflow/malformed ranges fail.
func thumbnailRange(value string, size int64) (start, end int64, partial bool, err error) {
	if size < 1 {
		return 0, 0, false, ErrThumbnail
	}
	end = size - 1
	if value == "" {
		return 0, end, false, nil
	}
	if len(value) > 128 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, ErrThumbnail
	}
	first, last, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	integer := func(s string) (int64, error) {
		if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, ErrThumbnail
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if !ok {
		return 0, 0, false, ErrThumbnail
	}
	if first == "" {
		n, e := integer(last)
		if e != nil || n < 1 {
			return 0, 0, false, ErrThumbnail
		}
		start = max(int64(0), size-n)
	} else {
		start, err = integer(first)
		if err != nil || start >= size {
			return 0, 0, false, ErrThumbnail
		}
		if last != "" {
			end, err = integer(last)
			if err != nil || end < start {
				return 0, 0, false, ErrThumbnail
			}
			end = min(end, size-1)
		}
	}
	return start, end, true, nil
}

func (b *thumbnailBridge) fail(ctx context.Context) {
	// FFmpeg closes an old streaming request when seeking. That cancellation is
	// normal; a live request failing proof/budget checks invalidates the session.
	if ctx.Err() == nil && b.ctx.Err() == nil {
		b.failed.Store(true)
		b.cancel()
	}
}

func (b *thumbnailBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Host != b.host || r.RequestURI != b.route || r.URL.Path != b.route || r.URL.Scheme != "" || r.URL.Host != "" || r.Header.Get("Origin") != "" || r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-r.Context().Done():
		return
	}
	b.budgetMu.Lock()
	b.requests++
	overBudget := b.requests > thumbnailRequestBudget
	b.budgetMu.Unlock()
	if overBudget || b.source.Current(r.Context()) != nil || b.ctx.Err() != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		b.fail(r.Context())
		return
	}
	start, end, partial, err := thumbnailRange(r.Header.Get("Range"), b.source.Size())
	if err != nil || len(r.Header.Values("Range")) > 1 {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(b.source.Size(), 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(b.source.Size(), 10))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	buffer := make([]byte, min(int64(thumbnailReadBlock), end-start+1))
	wroteHeader := false
	for at := start; at <= end; {
		count := min(int64(len(buffer)), end-at+1, int64(thumbnailReadBlock)-at%thumbnailReadBlock)
		b.budgetMu.Lock()
		overBudget := count > thumbnailReadBudget-b.bytes
		if !overBudget {
			b.bytes += count
		}
		b.budgetMu.Unlock()
		if overBudget {
			b.fail(r.Context())
			return
		}
		n, err := b.source.ReadAt(r.Context(), buffer[:count], at)
		if err != nil || int64(n) != count {
			b.fail(r.Context())
			return
		}
		if !wroteHeader {
			w.WriteHeader(status)
			wroteHeader = true
		}
		if _, err := w.Write(buffer[:n]); err != nil {
			return
		}
		at += int64(n)
	}
}
