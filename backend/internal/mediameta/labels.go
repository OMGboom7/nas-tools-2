package mediameta

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

// The builtin expressions retain the old release-group catalog, not an
// arbitrary suffix heuristic that could mistake codecs or years for teams.
const builtinReleaseGroups = `FF(?:(?:A|WE)B|CD|E(?:DU|B)|TV)|Audies|AD(?:Audio|E(?:|book)|Music|Web)|BeiTai|Bts(?:CHOOL|HD|PAD|TV)|Zone|CarPT|CHD(?:|Bits|PAD|(?:|HK)TV|WEB)|StBOX|OneHD|Lee|xiaopie|(?:(?:iNT|(?:HALFC|Mini(?:S|H|FH)D))-|)TLF|(?:DG|GBWE)B|Hares(?:|(?:M|T)V|Web)|HDA(?:pad|rea|TV)|EPiC|HDC(?:|hina|TV)|k9611|tudou|iHD|D(?:ream|BTV)|(?:HD|QHstudI)o|beAst(?:|TV)|HDH(?:|ome|Pad|TV|WEB)|HDPT(?:|Web)|HDS(?:|ky|TV|Pad|WEB)|AQLJ|HDZ(?:|one)|HHWEB|HTPT|FRDS|Yumi|cXcY|L(?:eague(?:(?:C|H)D|(?:M|T)V|NF|WEB)|HD)|i18n|CiNT|MTeam(?:|TV)|MPAD|Our(?:Bits|TV)|FLTTH|Ao|PbK|MGs|iLove(?:HD|TV)|PiGo(?:NF|(?:H|WE)B)|PTer(?:|DIY|Game|(?:M|T)V|WEB)|PTH(?:|Audio|eBook|music|ome|tv|WEB)|PTsbao|OPS|F(?:Fans(?:AIeNcE|BD|D(?:VD|IY)|TV|WEB)|HDMv)|SGXT|PuTao|CMCT(?:|V)|Shark(?:|WEB|DIY|TV|MV)|TJUPT|TTG|WiKi|NGB|DoA|(?:ARi|ExRE)N|B(?:MDru|eyondHD|TN)|C(?:fandora|trlhd|MRG)|DON|EVO|FLUX|HONE(?:|yG)|N(?:oGroup|T(?:b|G))|PandaMoon|SMURF|T(?:EPES|aengoo|rollHD )|ANi|HYSUB|KTXP|LoliHouse|MCE|Nekomoe kissaten|(?:Lilith|NC)-Raws|织梦字幕组`

type LabelOptions struct {
	ReleaseGroups, ReleaseSeparator string
	Customization, CustomSeparator  string
}

type settingReader interface {
	Get(context.Context, string) (string, error)
}

// ReadLabelOptions honors installed-plugin state and reads current persisted
// configuration; it never starts or calls the legacy plugin runtime.
func ReadLabelOptions(ctx context.Context, store settingReader) (LabelOptions, error) {
	options := LabelOptions{}
	raw, err := store.Get(ctx, "UserInstalledPlugins")
	if err != nil || raw == "" {
		return options, err
	}
	if len(raw) > 64<<10 {
		return options, errors.New("installed plugin list too large")
	}
	var installed []string
	if err := json.Unmarshal([]byte(raw), &installed); err != nil {
		return options, err
	}
	for _, id := range installed {
		if id != "CustomReleaseGroups" && id != "Customization" {
			continue
		}
		value, err := store.Get(ctx, "plugin."+id)
		if err != nil {
			return options, err
		}
		if value == "" {
			continue
		}
		if len(value) > 64<<10 {
			return options, errors.New("label configuration too large")
		}
		var config struct {
			ReleaseGroups string `json:"release_groups"`
			Customization string `json:"customization"`
			Separator     string `json:"separator"`
		}
		if err := json.Unmarshal([]byte(value), &config); err != nil {
			return options, err
		}
		if id == "CustomReleaseGroups" {
			options.ReleaseGroups, options.ReleaseSeparator = config.ReleaseGroups, config.Separator
		} else {
			options.Customization, options.CustomSeparator = config.Customization, config.Separator
		}
	}
	return options, nil
}

func MatchLabels(ctx context.Context, title string, options LabelOptions) (string, string, error) {
	if len(title) > 64<<10 || len(options.ReleaseGroups) > 16<<10 || len(options.Customization) > 16<<10 || len(options.ReleaseSeparator) > 128 || len(options.CustomSeparator) > 128 {
		return "", "", errors.New("label input too large")
	}
	groups := builtinReleaseGroups
	if options.ReleaseGroups != "" {
		custom := strings.ReplaceAll(strings.Trim(options.ReleaseGroups, ";"), ";", "|")
		custom = strings.ReplaceAll(custom, "\n", "|")
		groups += "|" + custom
	}
	release, err := regexcompat.Compile(`(?<=[-@\[￡【&])(?:`+groups+`)(?=[@.\s\]\[】&])`, regexp2.IgnoreCase)
	if err != nil {
		return "", "", err
	}
	var teams []string
	seen := map[string]bool{}
	outputBytes := 0
	err = eachLabelMatch(ctx, release, title+" ", func(match *regexp2.Match) error {
		value := match.String()
		if value != "" && !seen[value] {
			seen[value] = true
			teams = append(teams, value)
			outputBytes += len(value) + len(options.ReleaseSeparator) + 1
		}
		if outputBytes > 64<<10 {
			return errors.New("release labels too large")
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	separator := options.ReleaseSeparator
	if separator == "" {
		separator = "@"
	}
	team := strings.Join(teams, separator)
	if options.Customization == "" {
		return team, "", nil
	}
	patterns := strings.Split(strings.Trim(strings.ReplaceAll(options.Customization, "\n", ";"), ";"), ";")
	for index := range patterns {
		patterns[index] = "(" + patterns[index] + ")"
	}
	custom, err := regexcompat.Compile(strings.Join(patterns, "|"), 0)
	if err != nil {
		return "", "", err
	}
	type foundLabel struct {
		value string
		slot  int
	}
	var labels []foundLabel
	seen = map[string]bool{}
	outputBytes = 0
	err = eachLabelMatch(ctx, custom, title, func(match *regexp2.Match) error {
		for slot, group := range match.Groups()[1:] {
			value := group.String()
			if value != "" && !seen[value] {
				seen[value] = true
				labels = append(labels, foundLabel{value, slot})
				outputBytes += len(value) + len(options.CustomSeparator) + 1
				if len(labels) > 1000 || outputBytes > 64<<10 {
					return errors.New("custom labels too large")
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	sort.SliceStable(labels, func(i, j int) bool { return labels[i].slot < labels[j].slot })
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		values = append(values, label.value)
	}
	separator = options.CustomSeparator
	if separator == "" {
		separator = "@"
	}
	return team, strings.Join(values, separator), nil
}

func eachLabelMatch(ctx context.Context, pattern *regexp2.Regexp, title string, visit func(*regexp2.Match) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	match, err := pattern.FindStringMatch(title)
	for count := 0; match != nil && err == nil; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= 1000 || time.Since(started) > time.Second {
			return errors.New("label matching limit exceeded")
		}
		if err := visit(match); err != nil {
			return err
		}
		match, err = pattern.FindNextMatch(match)
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}
