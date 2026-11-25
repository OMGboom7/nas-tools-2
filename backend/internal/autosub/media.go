package autosub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/language"
)

var ErrMedia = errors.New("无法读取媒体流或生成字幕")
var ErrEngine = errors.New("语音识别引擎或模型不可用")

type Stream struct {
	Index       int
	Type        string `json:"codec_type"`
	Codec       string `json:"codec_name"`
	Width       *int
	Tags        struct{ Language string }
	Disposition struct{ Default, Forced int }
}

func Lang(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if alias := map[string]string{"chi": "zh", "fre": "fr", "ger": "de", "dut": "nl", "cze": "cs", "rum": "ro", "gre": "el", "may": "ms", "per": "fa", "ice": "is", "slo": "sk", "wel": "cy", "arm": "hy", "baq": "eu", "bur": "my", "geo": "ka", "mac": "mk", "mao": "mi", "tib": "bo", "alb": "sq"}[s]; alias != "" {
		return alias
	}
	b, err := language.ParseBase(s)
	if err != nil || len(b.String()) != 2 {
		return "und"
	}
	return b.String()
}
func Aliases(s string) []string {
	l := Lang(s)
	if l == "und" {
		return []string{"und"}
	}
	b, _ := language.ParseBase(l)
	out := []string{l, b.ISO3()}
	if l == "zh" {
		out = append(out, "chi")
	}
	if raw := strings.ToLower(s); len(raw) == 3 && Lang(raw) == l && !slices.Contains(out, raw) {
		out = append(out, raw)
	}
	return out
}
func Audio(streams []Stream) (Stream, bool) {
	var v Stream
	for _, s := range streams {
		if s.Type == "audio" && (v.Type == "" || s.Disposition.Default != 0) {
			v = s
		}
	}
	return v, v.Type != ""
}
func Subtitle(streams []Stream, preferred string) (Stream, int) {
	var v Stream
	best := -1
	count := 0
	for _, s := range streams {
		if s.Type != "subtitle" || s.Disposition.Forced != 0 || s.Width != nil || slices.Contains([]string{"dvd_subtitle", "dvb_subtitle", "hdmv_pgs_subtitle", "xsub"}, s.Codec) {
			continue
		}
		count++
		rank := 0
		if s.Disposition.Default != 0 {
			rank++
		}
		if Lang(s.Tags.Language) == preferred {
			rank += 2
		}
		if rank > best {
			best = rank
			v = s
		}
	}
	return v, count
}

type limitedOutput struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedOutput) Bytes() []byte { return b.buf.Bytes() }
func (b *limitedOutput) Write(v []byte) (int, error) {
	if b.buf.Len()+len(v) > b.limit {
		return 0, ErrMedia
	}
	return b.buf.Write(v)
}

// Both probe and extraction receive a seekable, read-only descriptor. Playlists
// and file/HTTP subresources are excluded from the demuxer's input protocols.
func mediaCommand(ctx context.Context, tool string, media *os.File, output io.Writer, args ...string) error {
	exe, err := exec.LookPath(tool)
	if err != nil || !filepath.IsAbs(exe) {
		return ErrMedia
	}
	if _, err = media.Seek(0, io.SeekStart); err != nil {
		return err
	}
	opts := []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2,mpegts,avi,mpeg,mpegvideo,flv,asf,ogg,rm", "-fd", "3", "-i", "fd:"}
	cmd := exec.CommandContext(ctx, exe, append(opts, args...)...)
	cmd.ExtraFiles = []*os.File{media}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err = cmd.Run(); err != nil {
		return ErrMedia
	}
	return ctx.Err()
}
func Probe(ctx context.Context, media *os.File) ([]Stream, error) {
	var out limitedOutput
	out.limit = 2 << 20
	err := mediaCommand(ctx, "ffprobe", media, &out, "-show_entries", "stream=index,codec_type,codec_name,width:stream_tags=language:stream_disposition=default,forced", "-of", "json")
	var doc struct{ Streams []Stream }
	if err != nil || json.Unmarshal(out.Bytes(), &doc) != nil || len(doc.Streams) == 0 {
		return nil, ErrMedia
	}
	return doc.Streams, nil
}
func Extract(ctx context.Context, media *os.File, track Stream) ([]byte, error) {
	var out limitedOutput
	out.limit = MaxSubtitle
	if err := mediaCommand(ctx, "ffmpeg", media, &out, "-map", "0:"+strconv.Itoa(track.Index), "-c:s", "srt", "-f", "srt", "pipe:1"); err != nil {
		return nil, err
	}
	if _, err := Parse(out.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type Whisper struct{ Binary, Model, Args string }

func (w Whisper) arguments() ([]string, error) {
	// Only an operator-installed executable is selectable from the web UI.
	trusted := os.Getenv("NASTOOL_WHISPER_CLI")
	if trusted == "" {
		trusted, _ = exec.LookPath("whisper-cli")
	}
	bin, err := filepath.EvalSymlinks(trusted)
	if err != nil || !filepath.IsAbs(bin) {
		return nil, ErrEngine
	}
	if w.Binary != "" {
		p, e := filepath.EvalSymlinks(w.Binary)
		if e != nil || p != bin {
			return nil, ErrEngine
		}
	}
	st, err := os.Stat(w.Model)
	if err != nil || !filepath.IsAbs(w.Model) || !st.Mode().IsRegular() {
		return nil, ErrEngine
	}
	argv := []string{bin}
	args := strings.Fields(w.Args)
	for i := 0; i < len(args); i++ {
		key := args[i]
		switch key {
		case "-tr", "--translate", "-di", "--diarize", "-tdrz", "--tinydiarize",
			"-ps", "--print-special", "-pc", "--print-colors", "-pp", "--print-progress", "-nt", "--no-timestamps", "-ls", "--log-score",
			"-debug", "--debug-mode", "-otxt", "--output-txt", "-ovtt", "--output-vtt", "-osrt", "--output-srt", "-olrc", "--output-lrc", "-ocsv", "--output-csv", "-oj", "--output-json", "-ojf", "--output-json-full",
			"-ng", "--no-gpu", "-fa", "--flash-attn", "-sow", "--split-on-word", "-nf", "--no-fallback", "-ntp", "--no-timestamps-printed":
			argv = append(argv, key)
		case "-ot", "--offset-t", "-on", "--offset-n", "-d", "--duration", "-t", "--threads", "-p", "--processors", "-bs", "--beam-size", "-bo", "--best-of", "-ml", "--max-len", "-mc", "--max-context", "-wt", "--word-thold", "-et", "--entropy-thold", "-lpt", "--logprob-thold", "-nth", "--no-speech-thold", "-tp", "--temperature", "-tpi", "--temperature-inc":
			i++
			if i == len(args) || !numericArg.MatchString(args[i]) {
				return nil, ErrEngine
			}
			argv = append(argv, key, args[i])
		case "--prompt", "-oved", "--ov-e-device":
			i++
			if i == len(args) || strings.HasPrefix(args[i], "-") || strings.ContainsAny(args[i], "\x00\r\n") {
				return nil, ErrEngine
			}
			argv = append(argv, key, args[i])
		default:
			return nil, fmt.Errorf("%w：请核对额外识别参数", ErrEngine)
		}
	}
	return argv, nil
}

var numericArg = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var detected = regexp.MustCompile(`auto-detected language:\s*([a-z]{2,3})\b`)

func (w Whisper) Recognize(ctx context.Context, media *os.File, audio Stream) ([]byte, string, error) {
	args, err := w.arguments()
	if err != nil {
		return nil, "", err
	}
	dir, err := os.MkdirTemp("", "nastool-autosub-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "audio.wav")
	// Let FFmpeg finalize the seekable WAV header in this private directory.
	if err := mediaCommand(ctx, "ffmpeg", media, io.Discard, "-map", "0:"+strconv.Itoa(audio.Index), "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", wav); err != nil {
		return nil, "", err
	}
	lang := Lang(audio.Tags.Language)
	if lang == "und" {
		lang = "auto"
	}
	argv := append(args[1:], "-l", lang, "-m", w.Model, "-osrt", "-of", wav, wav)
	cmd := exec.CommandContext(ctx, args[0], argv...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	log := &limitedOutput{limit: 4 << 20}
	cmd.Stderr = log
	cmd.Stdout = log
	if cmd.Run() != nil {
		return nil, "", ErrEngine
	}
	if slices.Contains(args[1:], "-tr") || slices.Contains(args[1:], "--translate") {
		lang = "en"
	} else if bytes.Contains(log.Bytes(), []byte("model is not multilingual, ignoring language and translation options")) {
		lang = "en"
	} else if lang == "auto" {
		m := detected.FindSubmatch(log.Bytes())
		if len(m) != 2 {
			return nil, "", fmt.Errorf("%w：无法确定语言", ErrEngine)
		}
		lang = Lang(string(m[1]))
	}
	f, err := os.Open(wav + ".srt")
	if err != nil {
		return nil, "", ErrEngine
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSubtitle+1))
	if err != nil {
		return nil, "", err
	}
	if _, err = Parse(data); err != nil {
		return nil, "", err
	}
	return data, lang, nil
}
