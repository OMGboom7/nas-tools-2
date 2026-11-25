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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var errNoSpeech = errors.New("音轨中未检测到可用语音")

// Whisper's byte-level BPE stores printable Unicode characters for all 256 bytes.
var tokenBytes = func() map[rune]byte {
	m := map[rune]byte{}
	extra := rune(256)
	for i := 0; i < 256; i++ {
		ch := rune(i)
		if !(i >= 33 && i <= 126 || i >= 161 && i <= 172 || i >= 174) {
			ch = extra
			extra++
		}
		m[ch] = byte(i)
	}
	return m
}()

type alignedSegment struct {
	Language   string
	Pieces     []string
	Times      []int64
	Start, End int64
}

func decodeCT2(data []byte) ([]byte, string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var header alignedSegment
	if dec.Decode(&header) != nil || len(header.Language) < 2 || len(header.Language) > 3 || strings.Trim(header.Language, "abcdefghijklmnopqrstuvwxyz") != "" {
		return nil, "", ErrEngine
	}
	lang := Lang(header.Language)
	var cues []Cue
	for {
		var s alignedSegment
		e := dec.Decode(&s)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil || s.Language != "" || len(s.Pieces) == 0 || len(s.Times) != len(s.Pieces)+1 || s.Start < 0 || s.End < s.Start {
			return nil, "", ErrEngine
		}
		var word strings.Builder
		start := s.Times[0]
		flush := func(end int64) {
			if txt := strings.TrimSpace(word.String()); txt != "" {
				cues = append(cues, Cue{start, end, strings.ToValidUTF8(txt, "�")})
			}
			word.Reset()
		}
		for i, piece := range s.Pieces {
			if s.Times[i] < 0 || s.Times[i+1] < s.Times[i] {
				return nil, "", ErrEngine
			}
			var raw strings.Builder
			for _, ch := range piece {
				v, ok := tokenBytes[ch]
				if !ok {
					return nil, "", ErrEngine
				}
				raw.WriteByte(v)
			}
			if lang == "en" && strings.HasPrefix(raw.String(), " ") && word.Len() > 0 && utf8.ValidString(word.String()) {
				flush(s.Times[i])
				start = s.Times[i]
			}
			word.WriteString(raw.String())
		}
		// Alignment gives each word its original time; non-English output stays
		// grouped by the model's segments, with their first/last aligned token.
		flush(s.Times[len(s.Times)-1])
	}
	if lang == "en" {
		cues = MergeEnglish(cues)
	}
	if len(cues) == 0 {
		return nil, "", errNoSpeech
	}
	out := Encode(cues)
	if len(out) > MaxSubtitle {
		return nil, "", ErrSubtitle
	}
	return out, lang, nil
}
func (m FasterWhisper) Recognize(ctx context.Context, media *os.File, audio Stream) ([]byte, string, error) {
	bin := os.Getenv("NASTOOL_CT2_WHISPER")
	if bin == "" {
		bin, _ = exec.LookPath("nastool-ct2-whisper")
	}
	if !filepath.IsAbs(bin) {
		return nil, "", errors.New("未找到原生 CTranslate2 识别程序")
	}
	files, err := m.resolve(ctx, modelClient)
	if err != nil {
		return nil, "", err
	}
	defer files.close()
	dir, err := os.MkdirTemp("", "nastool-ct2-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	pcm := filepath.Join(dir, "audio.pcm")
	if err := mediaCommand(ctx, "ffmpeg", media, io.Discard, "-map", "0:"+strconv.Itoa(audio.Index), "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "s16le", pcm); err != nil {
		return nil, "", err
	}
	lang := Lang(audio.Tags.Language)
	if lang == "und" {
		lang = "auto"
	}
	cmd := exec.CommandContext(ctx, bin, dir, pcm, lang)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	// The library can only open the previously selected descriptors through
	// this private model directory. Old snapshot links to cache/blobs work too.
	for i, item := range files {
		if e := os.Symlink(fmt.Sprintf("/dev/fd/%d", 3+i), filepath.Join(dir, item.name)); e != nil {
			return nil, "", e
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, item.file)
	}
	output := limitedOutput{limit: 4 * MaxSubtitle}
	cmd.Stdout = &output
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", errors.New("CTranslate2 识别失败，请核对模型、词表与 CPU 支持")
	}
	return decodeCT2(output.Bytes())
}
