package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestResultSeedingRequirements(t *testing.T) {
	for _, test := range []struct {
		name, seconds, ratio string
		wantError            bool
	}{
		{"unknown", "", "", false},
		{"explicit zero", "0", "0", false},
		{"HR", "259200", "2", false},
		{"ratio only", "", "0.8", false},
		{"time only", "90000", "", false},
		{"negative time", "-1", "1", true},
		{"fractional time", "0.5", "1", true},
		{"slash count is not time", "3/5", "1", true},
		{"overflow", "9223372036854775808", "1", true},
		{"bounded time", "1000000001", "1", true},
		{"invalid ratio", "1", "NaN", true},
		{"negative ratio", "1", "-0.5", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, options := resultFixture(t)
			var rules map[string]json.RawMessage
			if err := json.Unmarshal(plan.Definition.Torrents, &rules); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rules["fields"], &fields); err != nil {
				t.Fatal(err)
			}
			fields["minimumseedtime"] = json.RawMessage(`{"selector":".seedtime"}`)
			fields["minimumratio"] = json.RawMessage(`{"selector":".ratio"}`)
			rules["fields"], _ = json.Marshal(fields)
			plan.Definition.Torrents, _ = json.Marshal(rules)
			body := `<table class="torrents"><tr><td><a class="title">Movie</a><a class="download" href="download.php?id=1">download</a><span class="size">1 GB</span><span class="seedtime">` + test.seconds + `</span><span class="ratio">` + test.ratio + `</span></td></tr></table>`
			resources, err := ParseResults(context.Background(), plan, []byte(body), options)
			if test.wantError {
				if !errors.Is(err, ErrResponse) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || len(resources) != 1 {
				t.Fatal(resources, err)
			}
			r := resources[0]
			if (r.MinimumSeedTime == nil) != (test.seconds == "") || (r.MinimumRatio == nil) != (test.ratio == "") {
				t.Fatal(r)
			}
			if r.MinimumSeedTime != nil {
				encoded, _ := json.Marshal(*r.MinimumSeedTime)
				if string(encoded) != test.seconds {
					t.Fatal(string(encoded))
				}
			}
			if r.MinimumRatio != nil {
				encoded, _ := json.Marshal(*r.MinimumRatio)
				if string(encoded) != test.ratio {
					t.Fatal(string(encoded))
				}
			}
		})
	}
}

func TestBundledSeedingRequirementsWithoutPython(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(context.Background(), []byte(`<div><img title="H&R" class="hitandrun"><img title="Hit and Run"><div class="circle"><div class="circle-text">3</div></div></div>`))
	if err != nil {
		t.Fatal(err)
	}
	timeRules, ratioRules := 0, 0
	for _, definition := range catalog.Indexers {
		var rules torrentRules
		if json.Unmarshal(definition.Torrents, &rules) != nil {
			continue
		}
		selected := map[string]FieldSpec{}
		for _, key := range []string{"minimumseedtime", "minimumratio", "hr_days"} {
			if spec, exists := rules.Fields[key]; exists {
				selected[key] = spec
			}
		}
		if len(selected) == 0 {
			continue
		}
		values, err := ResolveFields(context.Background(), doc.Root, selected)
		if err != nil {
			t.Fatalf("%s: %v", definition.ID, err)
		}
		if _, exists := selected["minimumseedtime"]; exists {
			timeRules++
			value, err := resultSeedTime(values["minimumseedtime"])
			if err != nil || value == nil {
				t.Fatalf("%s: %v %v", definition.ID, value, err)
			}
			if definition.ID == "caihongdao" && *value != 259200 {
				t.Fatal(*value)
			}
		}
		if _, exists := selected["minimumratio"]; exists {
			ratioRules++
			value, err := resultFactor(values["minimumratio"])
			if err != nil || value == nil {
				t.Fatalf("%s: %v %v", definition.ID, value, err)
			}
			if definition.ID == "beibingyang" && *value != 0.8 {
				t.Fatal(*value)
			}
		}
	}
	if timeRules != 29 || ratioRules != 30 {
		t.Fatal(timeRules, ratioRules)
	}
}
