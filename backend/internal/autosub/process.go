package autosub

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type Options struct {
	Translate, ExtractOnly bool
	Engine                 string
	Whisper                Whisper
	Faster                 FasterWhisper
}

type Publication struct {
	Prepare func(organization.LibraryOutputProof) error
	Current func() error
}

// Process prefers an existing target, then a source sidecar, embedded text,
// and finally speech recognition. A newly appearing output always wins.
func Process(ctx context.Context, d organization.LibraryDefinition, m organization.LibraryMedia, opt Options, p Publication, translate TranslateFunc) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(m.Path), filepath.Ext(m.Path))
	find := func(language string) (string, error) {
		for _, code := range Aliases(language) {
			name := stem + "." + code + ".srt"
			exists, err := organization.LibraryExisting(ctx, d, m, "autosub", name)
			if err != nil {
				return "", err
			}
			if exists {
				return name, nil
			}
		}
		return "", nil
	}
	target := "en"
	if opt.Translate {
		target = "zh"
	}
	name, err := find(target)
	if err != nil {
		return "", err
	}
	if name != "" {
		return "已存在目标字幕", nil
	}
	var lang, existing, skip string
	data, err := organization.ReadLibraryMedia(ctx, d, m, func(f *os.File) ([]byte, error) {
		streams, err := Probe(ctx, f)
		if err != nil {
			return nil, err
		}
		if sub, n := Subtitle(streams, target); n > 0 && Lang(sub.Tags.Language) == target {
			skip = "已存在内嵌目标字幕"
			return nil, nil
		}
		audio, ok := Audio(streams)
		if !ok {
			return nil, ErrMedia
		}
		preferred := audio.Tags.Language
		if Lang(preferred) == "und" {
			preferred = "en"
		}
		existing, err = find(preferred)
		if err != nil {
			return nil, err
		}
		if existing != "" {
			lang = Lang(preferred)
			return organization.ReadAutoSubtitle(ctx, d, m, existing)
		}
		sub, n := Subtitle(streams, Lang(preferred))
		if n > 0 && (n == 1 || Lang(sub.Tags.Language) == Lang(preferred)) {
			lang = Lang(sub.Tags.Language)
			return Extract(ctx, f, sub)
		}
		if opt.ExtractOnly {
			skip = "无可提取字幕，已跳过"
			return nil, nil
		}
		if opt.Engine == "faster-whisper" {
			b, l, e := opt.Faster.Recognize(ctx, f, audio)
			if errors.Is(e, errNoSpeech) {
				skip = "音轨中未检测到语音，已跳过"
				return nil, nil
			}
			lang = l
			return b, e
		}
		if opt.Engine != "" && opt.Engine != "whisper.cpp" {
			return nil, errors.New("当前识别引擎尚未接入原生运行时")
		}
		b, l, e := opt.Whisper.Recognize(ctx, f, audio)
		lang = l
		return b, e
	})
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		if lang != "" || existing != "" {
			return "", ErrSubtitle
		}
		return skip, nil
	}
	if _, err := Parse(data); err != nil {
		return "", err
	}
	publish := func(language string, b []byte) (string, error) {
		_, state, err := organization.PublishLibrarySidecar(ctx, d, m, "autosub", stem+"."+language+".srt", b, p.Prepare, p.Current)
		return state, err
	}
	if existing == "" {
		state, e := publish(lang, data)
		if e != nil {
			return "", e
		}
		if state == "preserved" {
			if !opt.Translate || lang == "zh" {
				return "已有同名字幕，已保留", nil
			}
			saved, e := organization.ReadAutoSubtitle(ctx, d, m, stem+"."+lang+".srt")
			if e != nil {
				return "", e
			}
			if !bytes.Equal(saved, data) {
				return "已有同名字幕，已保留", nil
			}
		}
	}
	if !opt.Translate || lang == "zh" {
		return "已取得原文字幕", nil
	}
	if translate == nil {
		return "", errors.New("缺少字幕翻译服务")
	}
	translated, err := Bilingual(ctx, data, lang, translate)
	if err != nil {
		return "", err
	}
	if state, e := publish("zh", translated); e != nil {
		return "", e
	} else if state == "preserved" {
		return "已有同名中文字幕，已保留", nil
	}
	return "已生成双语字幕", nil
}
