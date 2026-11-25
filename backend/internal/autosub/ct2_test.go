//go:build darwin || linux

package autosub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCT2ReadsOriginalHubLayoutOffline(t *testing.T) {
	for _, owner := range []string{"Systran", "guillaumekln"} {
		dir := t.TempDir()
		ref := strings.Repeat("a", 40)
		repo := filepath.Join(dir, "cache", "models--"+owner+"--faster-whisper-tiny.en")
		snap := filepath.Join(repo, "snapshots", ref)
		for _, d := range []string{snap, filepath.Join(repo, "refs"), filepath.Join(repo, "blobs")} {
			if e := os.MkdirAll(d, 0700); e != nil {
				t.Fatal(e)
			}
		}
		if e := os.WriteFile(filepath.Join(repo, "refs", "main"), []byte(ref+"\n"), 0600); e != nil {
			t.Fatal(e)
		}
		for _, n := range []string{"model.bin", "config.json", "vocabulary.txt"} {
			if e := os.WriteFile(filepath.Join(repo, "blobs", n), []byte(n), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink("../../blobs/"+n, filepath.Join(snap, n)); e != nil {
				t.Fatal(e)
			}
		}
		client := &http.Client{Transport: modelTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("used network with a cached model")
			return nil, nil
		})}
		m := FasterWhisper{"tiny.en", dir}
		files, e := m.resolve(t.Context(), client)
		if e != nil {
			t.Fatal(e)
		}
		if len(files) != 3 {
			t.Fatal(len(files))
		}
		for _, f := range files {
			b, e := io.ReadAll(f.file)
			if e != nil || string(b) != f.name {
				t.Fatal(f.name, e)
			}
		}
		files.close()
		outside := filepath.Join(t.TempDir(), "weights")
		if e = os.WriteFile(outside, []byte("outside"), 0600); e != nil {
			t.Fatal(e)
		}
		if e = os.Remove(filepath.Join(snap, "model.bin")); e != nil {
			t.Fatal(e)
		}
		if e = os.Symlink(outside, filepath.Join(snap, "model.bin")); e != nil {
			t.Fatal(e)
		}
		if _, e = m.resolve(t.Context(), client); e == nil {
			t.Fatal("accepted escaped cache link")
		}
	}
}
func TestCT2DownloadsPublishOnlyCompleteVerifiedObjects(t *testing.T) {
	dir := t.TempDir()
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	data := "model fixture"
	sum := sha256.Sum256([]byte(data))
	obj := modelObject{int64(len(data)), hex.EncodeToString(sum[:])}
	body := "wrong content"
	client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	send := func() error {
		return downloadObject(context.Background(), client, root, "model.bin", "https://model.invalid/model.bin", obj)
	}
	if send() == nil {
		t.Fatal("accepted bad digest")
	}
	if _, e := root.Stat("model.bin"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("published invalid file")
	}
	body = data
	if e := send(); e != nil {
		t.Fatal(e)
	}
	if e := send(); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(dir, "model.bin"))
	if e != nil || string(got) != data {
		t.Fatal(e)
	}
	for _, name := range FasterModels() {
		m := ct2Models[name]
		if !revisionPattern.MatchString(m.Revision) || len(m.Files["model.bin"].Hash) != 64 || m.Files["config.json"].Bytes < 1 {
			t.Fatal(name)
		}
	}
}
func TestCT2TokenAlignmentAndBytePieces(t *testing.T) {
	if _, _, e := decodeCT2([]byte(`{"language":"en"}`)); !errors.Is(e, errNoSpeech) {
		t.Fatal(e)
	}
	b, lang, e := decodeCT2([]byte("{\"language\":\"en\"}\n" + `{"pieces":["ĠHel","lo","Ġthere","!","ĠGood","bye","."],"times":[0,60,150,340,400,560,660,700],"start":0,"end":700}`))
	if e != nil || lang != "en" {
		t.Fatal(lang, e)
	}
	c, e := Parse(b)
	if e != nil || len(c) != 2 || c[0] != (Cue{0, 400, "Hello there!"}) || c[1] != (Cue{400, 700, "Goodbye."}) {
		t.Fatal(c, e)
	}
	b, lang, e = decodeCT2([]byte("{\"language\":\"es\"}\n" + `{"pieces":["ĠÂ","¡Hola","!"],"times":[100,200,300,400],"start":100,"end":400}`))
	if e != nil || lang != "es" || !strings.Contains(string(b), "¡Hola!") {
		t.Fatal(string(b), e)
	}
	if _, _, e = decodeCT2([]byte(`{"language":"en"} {"pieces":["Ġbad"],"times":[3,1]}`)); !errors.Is(e, ErrEngine) {
		t.Fatal(e)
	}
}

func TestCT2InterruptedDownloadResumesAndRejectsCorruptedCache(t *testing.T) {
	dir := t.TempDir()
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	data := "fixture"
	hash := sha256.Sum256([]byte(data))
	obj := modelObject{int64(len(data)), hex.EncodeToString(hash[:])}
	count := 0
	client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
		count++
		res := &http.Response{StatusCode: 200, ContentLength: 7, Body: io.NopCloser(strings.NewReader("fix"))}
		if count == 2 {
			if r.Header.Get("Range") != "bytes=3-" {
				t.Fatal(r.Header)
			}
			res.StatusCode = 206
			res.ContentLength = 4
			res.Header = http.Header{"Content-Range": {"bytes 3-6/7"}}
			res.Body = io.NopCloser(strings.NewReader("ture"))
		}
		return res, nil
	})}
	send := func() error {
		return downloadObject(t.Context(), client, root, "model.bin", "https://example.invalid/model.bin", obj)
	}
	if send() == nil {
		t.Fatal("short response was published")
	}
	if e = send(); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("garbage"), 0600); e != nil {
		t.Fatal(e)
	}
	if !errors.Is(send(), errModel) || count != 2 {
		t.Fatal("corrupt installed file accepted or replaced")
	}
}
func TestCT2OriginalWeightsInNativeProcess(t *testing.T) {
	model := os.Getenv("NASTOOL_CT2_TEST_MODEL")
	source := os.Getenv("NASTOOL_CT2_TEST_MEDIA")
	if model == "" || source == "" {
		t.Skip("requires the original tiny.en CTranslate2 weights and public JFK fixture")
	}
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	cfg := FasterWhisper{"tiny.en", t.TempDir()}
	calls := 0
	client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
		prefix := "/Systran/faster-whisper-tiny.en/resolve/" + ct2Models["tiny.en"].Revision + "/"
		if r.URL.Scheme != "https" || r.URL.Host != "huggingface.co" || !strings.HasPrefix(r.URL.Path, prefix) {
			t.Fatalf("unexpected model request %s", r.URL)
		}
		calls++
		name := filepath.Base(r.URL.Path)
		f, e := os.Open(filepath.Join(model, name))
		if e != nil {
			return nil, e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, e
		}
		return &http.Response{StatusCode: 200, ContentLength: st.Size(), Body: f}, nil
	})}
	files, e := cfg.resolve(t.Context(), client)
	if e != nil {
		t.Fatal(e)
	}
	files.close()
	if calls != 3 {
		t.Fatal(calls)
	}
	f, e := os.Open(source)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	streams, e := Probe(t.Context(), f)
	if e != nil {
		t.Fatal(e)
	}
	track, ok := Audio(streams)
	if !ok {
		t.Fatal("missing audio")
	}
	b, l, e := cfg.Recognize(t.Context(), f, track)
	if e != nil || l != "en" || !strings.Contains(strings.ToLower(string(b)), "country") {
		t.Fatal(string(b), l, e)
	}
}
func TestWhisperAcceptsOriginalRecognitionAndPrivateOutputFlags(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "whisper-cli")
	if e := os.WriteFile(binary, []byte("test fixture"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("NASTOOL_WHISPER_CLI", binary)
	w := Whisper{Binary: binary, Model: binary, Args: "-ot 100 -d 5000 --print-progress --tinydiarize --output-json --prompt Dialogue"}
	argv, e := w.arguments()
	if e != nil || strings.Join(argv[1:], " ") != w.Args {
		t.Fatal(argv, e)
	}
	w.Args = "--output-file /other/file"
	if _, e = w.arguments(); e == nil {
		t.Fatal("allowed another output path")
	}
}

func TestCT2KilledDownloaderReleasesLockAndResumesPrefix(t *testing.T) {
	const marker = "NASTOOL_TEST_CT2_PARTIAL"
	const split = 64 << 10
	data := strings.Repeat("native", 32<<10)
	h := sha256.Sum256([]byte(data))
	obj := modelObject{int64(len(data)), hex.EncodeToString(h[:])}
	if dir := os.Getenv(marker); dir != "" {
		root, e := os.OpenRoot(dir)
		if e != nil {
			t.Fatal(e)
		}
		defer root.Close()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: obj.Bytes, Body: io.NopCloser(io.MultiReader(strings.NewReader(data[:split]), reader))}, nil
		})}
		// Keep the response open until the parent terminates this process.
		timer := time.AfterFunc(time.Minute, func() { writer.CloseWithError(io.ErrUnexpectedEOF) })
		defer timer.Stop()
		t.Fatal(downloadObject(t.Context(), client, root, "weights", "https://fixture.invalid", obj))
	}
	dir := t.TempDir()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	child := exec.Command(exe, "-test.run=^TestCT2KilledDownloaderReleasesLockAndResumesPrefix$")
	child.Env = append(os.Environ(), marker+"="+dir)
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if st, e := os.Stat(filepath.Join(dir, ".weights.part")); e == nil && st.Size() == split {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("partial download never reached disk")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = child.Wait(); e == nil {
		t.Fatal("child was not interrupted")
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", split) {
			t.Fatal(r.Header)
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", split, len(data)-1, len(data))}}, ContentLength: int64(len(data) - split), Body: io.NopCloser(strings.NewReader(data[split:]))}, nil
	})}
	if e = downloadObject(t.Context(), client, root, "weights", "https://fixture.invalid", obj); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(dir, "weights"))
	if e != nil || string(got) != data {
		t.Fatal("incomplete recovered model", e)
	}
}
