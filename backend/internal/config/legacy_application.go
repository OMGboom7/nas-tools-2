package config

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// These changes stay within config.yaml and are published with the cleanup
// of imported database settings so both retain the same pre-upgrade backup.
type legacyApplication struct {
	RefreshDefault bool
	OldTMDB        bool
	SetTMDB        bool
	OldBaidu       bool
	OCR            map[string]any
}

func (a legacyApplication) empty() bool {
	return !a.RefreshDefault && !a.OldTMDB && !a.OldBaidu && len(a.OCR) == 0
}

func readLegacyApplication(root *yaml.Node) (legacyApplication, error) {
	var data struct {
		App   map[string]any `yaml:"app"`
		Lab   map[string]any `yaml:"laboratory"`
		PT    any            `yaml:"pt"`
		OCR   any            `yaml:"ocr"`
		Baidu any            `yaml:"baiduocr"`
	}
	var plan legacyApplication
	if err := root.Decode(&data); err != nil {
		return plan, errors.New("invalid legacy application settings")
	}
	if pt, ok := data.PT.(map[string]any); ok {
		_, present := pt["ptrefresh_date_cron"]
		plan.RefreshDefault = !present
	}
	plan.OldTMDB = !emptyLegacySetting(data.Lab["tmdb_proxy"])
	plan.SetTMDB = plan.OldTMDB && emptyLegacySetting(data.App["tmdb_domain"])
	ocr, ok := data.OCR.(map[string]any)
	if !ok && !emptyLegacySetting(data.OCR) {
		return plan, errors.New("invalid OCR settings")
	}
	plan.OCR = map[string]any{}
	if len(ocr) == 0 {
		for _, key := range []string{"custom_ocr_url", "baiduocr_api_key", "baiduocr_secret_key"} {
			plan.OCR[key] = ""
		}
	}
	if !emptyLegacySetting(data.Baidu) {
		old, ok := data.Baidu.(map[string]any)
		if !ok {
			return plan, errors.New("invalid legacy Baidu OCR settings")
		}
		plan.OldBaidu = true
		// Preserve a current credential pair, including a deliberate clearing.
		// Never combine credentials belonging to different configurations.
		if ocr["baiduocr_api_key"] == nil && ocr["baiduocr_secret_key"] == nil {
			for _, key := range []string{"api_key", "secret_key"} {
				raw := old[key]
				if raw == nil {
					raw = ""
				}
				s, ok := raw.(string)
				if !ok {
					return plan, errors.New("invalid legacy Baidu OCR credential")
				}
				plan.OCR["baiduocr_"+key] = s
			}
		}
	}
	return plan, nil
}
