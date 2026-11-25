//go:build linux || darwin

package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func TestAutoSubNativeRunKeepsSourceAndRecoversTranslation(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, e := exec.LookPath(tool); e != nil {
			t.Skip(tool + " unavailable")
		}
	}
	f := newIyuuFixture(t)
	dir := t.TempDir()
	ctx := t.Context()
	write := func(p, data string) {
		t.Helper()
		if e := os.WriteFile(p, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	const source = "1\n00:00:00,000 --> 00:00:01,000\nFixture words.\n\n2\n00:00:01,001 --> 00:00:01,400\n[music]\n"
	input := filepath.Join(t.TempDir(), "input.srt")
	write(input, source)
	mkv := func(name string, embedded bool) {
		args := []string{"-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1.5"}
		if embedded {
			args = append(args, "-f", "srt", "-i", input, "-map", "0:a", "-map", "1:s", "-c:s", "srt", "-metadata:s:s:0", "language=eng")
		}
		args = append(args, "-c:a", "pcm_s16le", "-metadata:s:a:0", "language=eng", filepath.Join(dir, name+".mkv"))
		if b, e := exec.Command("ffmpeg", args...).CombinedOutput(); e != nil {
			t.Fatalf("%s: %v", b, e)
		}
	}
	mkv("Embedded", true)
	mkv("External", false)
	write(filepath.Join(dir, "External.eng.srt"), source)
	write(filepath.Join(dir, "Keep.mkv"), "old movie")
	write(filepath.Join(dir, "Keep.chi.srt"), "untouched")
	clipDir := filepath.Join(dir, "Disc", "BDMV", "STREAM")
	if e := os.MkdirAll(clipDir, 0700); e != nil {
		t.Fatal(e)
	}
	if b, e := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=220:duration=1.5", "-c:a", "mp2", "-f", "mpegts", filepath.Join(clipDir, "00001.m2ts")).CombinedOutput(); e != nil {
		t.Fatalf("%s: %v", b, e)
	}
	write(filepath.Join(clipDir, "00001.eng.srt"), source)
	b, e := os.ReadFile(f.config)
	if e != nil {
		t.Fatal(e)
	}
	write(f.config, string(b)+"\nopenai:\n  api_url: https://translate.fixture\n  api_key: fixture-token\n")
	calls := 0
	mock := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if r.URL.String() != "https://translate.fixture/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-token" || !strings.Contains(string(b), "Fixture words.") || strings.Contains(string(b), dir) || strings.Contains(string(b), "[music]") {
			t.Errorf("unexpected translation request")
		}
		return jsonResponse(r, `{"choices":[{"message":{"content":"临时译文。"}}]}`), nil
	})
	restart := func() *autoSubAPI {
		var runner *rssRunAPI
		h, e := buildHandler(config.Config{ApplicationConfigPath: f.config, DisableLegacy: true}, mock, &runner)
		if e != nil {
			t.Fatal(e)
		}
		f.handler = h
		f.token = loginForTest(t, h, "admin", "password")
		return runner.archivePlugin.autoSub
	}
	api := restart()
	cfg, _ := json.Marshal(map[string]any{"path_list": dir, "file_size": 0, "translate_zh": true, "translate_only": true, "run_now": true, "send_notify": false, "future": json.RawMessage("9007199254740993")})
	if _, e = f.db.Exec("UPDATE SYSTEM_DICT SET VALUE='[\"AutoSub\"]' WHERE KEY='UserInstalledPlugins'; INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.AutoSub',?)", string(cfg)); e != nil {
		t.Fatal(e)
	}
	tick := func() {
		t.Helper()
		if e := api.tick(ctx, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	tick()
	tick()
	page := func() string { return f.request("GET", "/api/v1/plugins/AutoSub/page", nil, 200).Body.String() }
	if out := page(); !strings.Contains(out, "已生成双语字幕") || !strings.Contains(out, "失败 0 项") {
		t.Fatal(out)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	read := func(n string) string {
		b, e := os.ReadFile(filepath.Join(dir, n))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	if read("External.eng.srt") != source || read("Keep.chi.srt") != "untouched" {
		t.Fatal("source or existing subtitle was overwritten")
	}
	for _, n := range []string{"Embedded.zh.srt", "External.zh.srt", "Disc/BDMV/STREAM/00001.zh.srt"} {
		got := read(n)
		if !strings.Contains(got, "临时译文。\nFixture words.") || !strings.Contains(got, "[music]") {
			t.Fatal(got)
		}
	}
	raw, e := api.store.Get(ctx, autoSubConfig)
	if e != nil || !strings.Contains(raw, "9007199254740993") || !strings.Contains(raw, `"run_now":false`) {
		t.Fatal(raw, e)
	}
	// Simulate losing the final file-state commit after publication.
	raw, e = api.store.Get(ctx, autoSubControl)
	if e != nil {
		t.Fatal(e)
	}
	s, e := readDiskRun(raw)
	if e != nil {
		t.Fatal(e)
	}
	s.Status = "queued"
	b, _ = json.Marshal(s)
	if _, e = f.db.Exec("UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY='GoAutoSub'; UPDATE GO_AUTOSUB_FILES SET state='running' WHERE path LIKE '%Embedded.mkv'", string(b)); e != nil {
		t.Fatal(e)
	}
	api = restart()
	tick()
	if calls != 3 || !strings.Contains(page(), "已核对中断前生成的字幕") {
		t.Fatal(calls, page())
	}
}
func TestAutoSubTranslationUnknownIsNotReplayed(t *testing.T) {
	f := newIyuuFixture(t)
	calls := 0
	var runner *rssRunAPI
	_, e := buildHandler(config.Config{ApplicationConfigPath: f.config, DisableLegacy: true}, roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF }), &runner)
	if e != nil {
		t.Fatal(e)
	}
	a := runner.archivePlugin.autoSub
	ctx := t.Context()
	if e = a.store.Transaction(ctx, func(tx *sql.Tx) error { return autoSubTables(ctx, tx) }); e != nil {
		t.Fatal(e)
	}
	c := map[string]any{"openai": map[string]any{"api_url": "https://fixture.invalid", "api_key": "fiction"}}
	for i := 0; i < 2; i++ {
		_, e = a.translate(ctx, "job", 1, c, "words", func() error { return nil })
		if !errors.Is(e, errTranslationUnknown) {
			t.Fatal(e)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestAutoSubRetryRequiresBothPublicationNamesAbsent(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "Movie.mkv"), []byte("media"), 0600); e != nil {
		t.Fatal(e)
	}
	root, e := organization.FreezeDiskRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	d := organization.AutoSubtitleLibrary(root)
	m, e := organization.CaptureLibraryMedia(ctx, d, "Movie.mkv")
	if e != nil {
		t.Fatal(e)
	}
	var proof organization.LibraryOutputProof
	stop := errors.New("stop before writing")
	_, _, e = organization.PublishLibrarySidecar(ctx, d, m, "autosub", "Movie.en.srt", []byte("test"), func(p organization.LibraryOutputProof) error { proof = p; return stop }, func() error { return nil })
	if !errors.Is(e, stop) || !organization.AutoSubtitleStageGone(ctx, d, m, proof) {
		t.Fatal(proof, e)
	}
	if e = os.Symlink(filepath.Join(dir, "Movie.mkv"), filepath.Join(dir, "Movie.en.srt")); e != nil {
		t.Fatal(e)
	}
	if organization.AutoSubtitleStageGone(ctx, d, m, proof) {
		t.Fatal("unsafe retry with occupied destination")
	}
	if _, e = organization.ReadAutoSubtitle(ctx, d, m, "Movie.en.srt"); e == nil {
		t.Fatal("followed a sidecar symlink")
	}
	for _, bad := range []string{"../Other.en.srt", "Movie.nfo", "Other.en.srt", "Movie...srt"} {
		if _, e = organization.LibraryTarget(d, m, "autosub", bad); e == nil {
			t.Fatal(bad)
		}
	}
}
