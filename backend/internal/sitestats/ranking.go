package sitestats

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RankStatus struct {
	Configured              bool              `json:"configured"`
	Site                    string            `json:"site"`
	CurrentLevel            string            `json:"current_level"`
	NextLevel               *string           `json:"next_level"`
	OverallProgress         float64           `json:"overall_progress"`
	Requirements            []RankRequirement `json:"requirements"`
	LevelSteps              []RankStep        `json:"level_steps"`
	KeepAccountLevel        string            `json:"keep_account_level"`
	CurrentIsKeep           bool              `json:"current_is_keep"`
	RuleURL                 string            `json:"rule_url"`
	IsSpecialLevel          bool              `json:"is_special_level"`
	SpecialLevelDescription string            `json:"special_level_description"`
	StatusType              string            `json:"status_type"`
	Message                 string            `json:"message"`
}

type RankRequirement struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Current  float64 `json:"current"`
	Required float64 `json:"required"`
	Progress float64 `json:"progress"`
	Met      bool    `json:"met"`
	Need     float64 `json:"need"`
	Unit     string  `json:"unit"`
}

type RankStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Order  int    `json:"order"`
	IsKeep bool   `json:"is_keep"`
}

type rankLevel struct {
	Name         string          `json:"name"`
	Order        *int            `json:"order"`
	Description  string          `json:"description"`
	Requirements json.RawMessage `json:"requirements"`
}

func (l *rankLevel) order() int {
	if l.Order == nil {
		return 0
	}
	return *l.Order
}

var (
	levelAlt    = regexp.MustCompile(`(?i)alt\s*=\s*["']([^"']+)["']`)
	levelTitle  = regexp.MustCompile(`(?i)title\s*=\s*["']([^"']+)["']`)
	levelTag    = regexp.MustCompile(`<[^>]*>`)
	levelPrefix = regexp.MustCompile(`^[（(][^）)]*[）)]\s*`)
	levelSuffix = regexp.MustCompile(`\s*[（(][^）)]*[）)]\s*$`)
)

// NormalizeLevel accepts the old stored icon markup and parenthetical labels.
func NormalizeLevel(raw string) string {
	attribute := levelAlt.FindStringSubmatch(raw)
	if attribute == nil {
		attribute = levelTitle.FindStringSubmatch(raw)
	}
	if attribute != nil {
		raw = attribute[1]
	}
	raw = levelTag.ReplaceAllString(raw, "")
	raw = levelPrefix.ReplaceAllString(raw, "")
	return strings.TrimSpace(levelSuffix.ReplaceAllString(raw, ""))
}

func sameLevel(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func findRankLevel(levels []rankLevel, name string) *rankLevel {
	if name == "" {
		return nil
	}
	for i := range levels {
		if levels[i].Name == name {
			return &levels[i]
		}
	}
	for i := range levels {
		if sameLevel(levels[i].Name, name) {
			return &levels[i]
		}
	}
	return nil
}

func rankRuleURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

// Rank projects the original catalog rules over one stored statistics row.
// It does not change the saved level or infer a promotion from met conditions.
func Rank(item Snapshot, raw json.RawMessage, now time.Time) (RankStatus, error) {
	out := RankStatus{
		Site: item.Site, CurrentLevel: NormalizeLevel(item.Level), Requirements: []RankRequirement{},
		LevelSteps: []RankStep{}, StatusType: "not_configured", Message: "站点未配置等级数据",
	}
	var rules struct {
		Enabled bool   `json:"enabled"`
		Keep    string `json:"keep_account_level"`
		URL     string `json:"rule_url"`
		System  struct {
			Levels  []rankLevel `json:"levels"`
			Special []rankLevel `json:"special_levels"`
		} `json:"ranking_system"`
	}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		return out, err
	}
	if !rules.Enabled {
		return out, nil
	}
	out.Configured, out.KeepAccountLevel, out.RuleURL = true, rules.Keep, rankRuleURL(rules.URL)
	levels := rules.System.Levels
	current := findRankLevel(levels, out.CurrentLevel)
	keep := findRankLevel(levels, rules.Keep)
	if current != nil && keep != nil {
		out.CurrentIsKeep = current.order() >= keep.order()
	} else {
		out.CurrentIsKeep = strings.TrimSpace(rules.Keep) != "" && sameLevel(out.CurrentLevel, rules.Keep)
	}
	sorted := make([]*rankLevel, 0, len(levels))
	for i := range levels {
		sorted = append(sorted, &levels[i])
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].order() < sorted[j].order() })
	var next *rankLevel
	switch special := findRankLevel(rules.System.Special, out.CurrentLevel); {
	case special != nil:
		out.StatusType, out.Message, out.OverallProgress = "special_level", "特殊等级不参与晋升", 100
		out.IsSpecialLevel, out.SpecialLevelDescription = true, special.Description
	case current == nil:
		out.StatusType, out.Message = "mapping_error", "当前等级未在映射中配置"
	default:
		for _, level := range sorted {
			if level.order() > current.order() {
				next = level
				break
			}
		}
		out.OverallProgress = 100
		if next == nil {
			out.StatusType, out.Message = "highest_level", "已达到最高等级"
		} else {
			out.StatusType, out.Message, out.NextLevel = "normal", "", &next.Name
			requirements, err := rankRequirements(next.Requirements, item, now)
			if err != nil {
				return out, err
			}
			if len(requirements) > 0 {
				var total float64
				for _, condition := range requirements {
					total += condition.Progress
				}
				out.Requirements, out.OverallProgress = requirements, rankRound(total/float64(len(requirements)))
			}
		}
	}
	currentIndex := slices.Index(sorted, current)
	for i, level := range sorted {
		step := RankStep{Name: level.Name, State: "locked", Order: i}
		if step.Name == "" {
			step.Name = "-"
		}
		if level.Order != nil {
			step.Order = *level.Order
		}
		step.IsKeep = strings.TrimSpace(rules.Keep) != "" && sameLevel(step.Name, rules.Keep)
		switch {
		case level == current:
			step.State = "current"
		case i < currentIndex:
			step.State = "done"
		case level == next:
			step.State = "target"
		}
		out.LevelSteps = append(out.LevelSteps, step)
	}
	return out, nil
}

func rankRequirements(raw json.RawMessage, item Snapshot, now time.Time) ([]RankRequirement, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	// Retain the order in which the original catalog presents its conditions.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, ErrResponse
	}
	var out []RankRequirement
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrResponse
		}
		var rawValue any
		if err = decoder.Decode(&rawValue); err != nil {
			return nil, err
		}
		required := rankNumber(rawValue)
		label, unit, current := rankMetric(key, item, now)
		progress := 100.0
		if required > 0 {
			progress = min(max(current/required, 0), 1) * 100
		}
		need := max(required-current, 0)
		if math.IsInf(need, 1) {
			need = math.MaxFloat64
		}
		out = append(out, RankRequirement{key, label, rankRound(current), rankRound(required), rankRound(progress), progress >= 100, rankRound(need), unit})
	}
	return out, nil
}

func rankNumber(value any) float64 {
	var text string
	switch v := value.(type) {
	case json.Number:
		text = v.String()
	case string:
		text = strings.TrimSpace(v)
	case bool:
		if v {
			return 1
		}
	}
	n, _ := strconv.ParseFloat(text, 64)
	return finiteRankNumber(n)
}

func finiteRankNumber(value float64) float64 {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return 0
	}
	return value
}

func rankRound(n float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(n, 'f', 2, 64), 64)
	return v
}

func rankMetric(key string, item Snapshot, now time.Time) (string, string, float64) {
	switch key {
	case "weeks":
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02", "2006/01/02 15:04:05", "2006/01/02"} {
			if joined, err := time.Parse(layout, strings.TrimSpace(item.Joined)); err == nil {
				// The original compares local wall-clock dates without zone offsets.
				civil := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC)
				days := math.Floor(float64(civil.Unix()-joined.Unix()) / 86400)
				return "Weeks", "weeks", max(days, 0) / 7
			}
		}
		return "Weeks", "weeks", 0
	case "download_gb":
		return "Download", "GiB", float64(item.Download) / (1 << 30)
	case "ratio":
		ratio := item.Ratio
		if ratio == 0 || ratio > 10000 || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
			ratio = 10000
		}
		return "Ratio", "", ratio
	case "bonus":
		return "Bonus", "", finiteRankNumber(item.Bonus)
	}
	metrics := map[string]int64{
		"upload": item.Upload, "download": item.Download, "seeding": item.Seeding, "leeching": item.Leeching,
		"seeding_size": item.SeedingSize, "msg_unread": item.Unread,
	}
	return key, "", float64(metrics[key])
}
