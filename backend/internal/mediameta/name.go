package mediameta

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Metadata struct {
	Title, Year                                        string
	Resolution, Source, VideoCodec, AudioCodec, Effect string
	Episodes                                           EpisodeInfo
	Team, Customization                                string
}

var (
	leadingGroup    = regexp.MustCompile(`^\s*(?:\[[^\]]+\]|【[^】]+】)\s*`)
	mediaExtension  = regexp.MustCompile(`(?i)\.(?:mkv|mp4|avi|mov|wmv|ts|m2ts|iso|rmvb|flv|webm)$`)
	yearToken       = regexp.MustCompile(`(?:^|[ ._\[(])((?:19|20)[0-9]{2})`)
	resolutionToken = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])((?:[0-9]{3,4}[pi])|[248]k)(?:[^a-z0-9]|$)`)
	sourceToken     = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(BluRay|Blu-Ray|WEB[ ._-]?DL|WEBRip|HDTV|UHDTV|HDDVD|DVDRip|BDRip|HDRip|WEB|BD)(?:[^a-z0-9]|$)`)
	videoToken      = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([hx][ .]?26[45]|AVC|HEVC|AV1|VC[ .]?1|MPEG[ .]?[24]|Xvid|DivX)(?:[^a-z0-9]|$)`)
	audioToken      = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(DTS[ ._-]?HD[ ._-]?MA|DTS[ ._-]?HD|TrueHD|Atmos|DTS|AC3|DDP|DD|LPCM|AAC|FLAC)(?:[^a-z0-9]|$)`)
	effectToken     = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(REMUX|UHD|SDR|HDR10\+?|HDR|DOVI|DV|3D|REPACK)(?:[^a-z0-9]|$)`)
	nameSeasonToken = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:S[0-9]{1,2}(?:E|[^a-z0-9]|$)|EP?[0-9]{1,4}(?:[^a-z0-9]|$)|Season\s+[0-9]{1,2}\b)|(?:第|全|共)\s*[0-9零〇一二两三四五六七八九十百千]+(?:\s*-\s*[0-9零〇一二两三四五六七八九十百千]+)?\s*[季集话話期]|[0-9零〇一二两三四五六七八九十百千]+\s*集\s*全`)
	nameSpaces      = regexp.MustCompile(`[ ._]+`)
)

// Parse extracts offline metadata after custom-word processing. It deliberately
// does not fabricate TMDB identities or categories from an unverified title.
func Parse(title, subtitle string) (Metadata, error) {
	return ParseWithOptions(context.Background(), title, subtitle, LabelOptions{})
}

func ParseWithOptions(ctx context.Context, title, subtitle string, options LabelOptions) (Metadata, error) {
	if len(title) > 64<<10 || len(subtitle) > 64<<10 || !utf8.ValidString(title) || !utf8.ValidString(subtitle) {
		return Metadata{}, errors.New("invalid release title")
	}
	episodes, err := Episodes(title, subtitle)
	if err != nil {
		return Metadata{}, err
	}
	metadata := Metadata{Episodes: episodes}
	metadata.Team, metadata.Customization, err = MatchLabels(ctx, title, options)
	if err != nil {
		return Metadata{}, err
	}
	clean := mediaExtension.ReplaceAllString(leadingGroup.ReplaceAllString(strings.TrimSpace(title), ""), "")
	cutoff := len(clean)
	for _, field := range []struct {
		pattern *regexp.Regexp
		target  *string
	}{
		{resolutionToken, &metadata.Resolution}, {sourceToken, &metadata.Source}, {videoToken, &metadata.VideoCodec}, {audioToken, &metadata.AudioCodec}, {effectToken, &metadata.Effect},
	} {
		if match := field.pattern.FindStringSubmatchIndex(clean); match != nil {
			*field.target = clean[match[2]:match[3]]
			if match[2] > 0 && match[2] < cutoff {
				cutoff = match[2]
			}
		}
	}
	var yearMatch []int
	for _, match := range yearToken.FindAllStringSubmatchIndex(clean, -1) {
		if match[2] > 0 && match[2] < cutoff && (match[3] == len(clean) || strings.ContainsRune(" ._])", rune(clean[match[3]]))) {
			yearMatch = match
		}
	}
	if yearMatch != nil {
		metadata.Year = clean[yearMatch[2]:yearMatch[3]]
		cutoff = yearMatch[2]
	}
	if match := nameSeasonToken.FindStringIndex(clean); match != nil && match[0] < cutoff {
		cutoff = match[0]
	}
	if match := animeEpisodePattern.FindStringIndex(clean); match != nil && match[0] < cutoff {
		cutoff = match[0]
	}
	for _, pattern := range []*regexp.Regexp{chainedEpisodePattern, englishEpisodePattern, bracketEpisodePattern} {
		if match := pattern.FindStringIndex(clean); match != nil && match[0] < cutoff {
			cutoff = match[0]
		}
	}
	metadata.Title = strings.Trim(nameSpaces.ReplaceAllString(clean[:cutoff], " "), " \t\r\n()[]【】-–")
	metadata.Resolution = strings.ToLower(metadata.Resolution)
	switch strings.ToUpper(strings.NewReplacer("-", "", " ", "", ".", "", "_", "").Replace(metadata.Source)) {
	case "WEB", "WEBDL":
		metadata.Source = "WEB-DL"
	case "BLURAY":
		metadata.Source = "BluRay"
	}
	return metadata, nil
}
