package sitestats

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

const fixtureRanking = `{
  "enabled":true,"keep_account_level":"Power User","rule_url":"https://rules.fixture.test/wiki",
  "ranking_system":{
    "levels":[
      {"name":"Top","order":10},
      {"name":"User","order":1},
      {"name":"Power User","order":2,"requirements":{"weeks":4,"download_gb":2,"bonus":200,"ratio":"2","seeding":4}},
      {"name":"Alternate","order":2}
    ],
    "special_levels":[{"name":"VIP","description":"永久会员"}]
  }
}`

func TestRankingComputesRequirementsFromStoredStatistics(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.FixedZone("fixture", 8*3600))
	item := Snapshot{Site: "Fixture", Level: `<img title="alternate" ALT="(站名) User（备注）">`, Joined: "2026/10/21 12:00:00", Download: 1 << 30, Bonus: 100, Ratio: 0, Seeding: 1}
	rank, err := Rank(item, json.RawMessage(fixtureRanking), now)
	if err != nil || rank.StatusType != "normal" || rank.CurrentLevel != "User" || rank.NextLevel == nil ||
		*rank.NextLevel != "Power User" || rank.OverallProgress != 55 || rank.CurrentIsKeep || rank.RuleURL != "https://rules.fixture.test/wiki" {
		t.Fatalf("rank = %+v, err = %v", rank, err)
	}
	keys := []string{"weeks", "download_gb", "bonus", "ratio", "seeding"}
	want := []float64{2, 1, 100, 10000, 1}
	if len(rank.Requirements) != len(keys) {
		t.Fatal(rank.Requirements)
	}
	for i, requirement := range rank.Requirements {
		if requirement.Key != keys[i] || requirement.Current != want[i] || requirement.Met != (i == 3) {
			t.Fatal(rank.Requirements)
		}
	}
	if len(rank.LevelSteps) != 4 || rank.LevelSteps[0].State != "current" ||
		rank.LevelSteps[1].State != "target" || !rank.LevelSteps[1].IsKeep || rank.LevelSteps[2].State != "locked" {
		t.Fatal(rank)
	}
}

func TestRankingDistinguishesSpecialHighestUnmappedAndDisabled(t *testing.T) {
	for _, test := range []struct {
		level, raw, state string
		progress          float64
		keep              bool
	}{
		{"vip", fixtureRanking, "special_level", 100, false},
		{" Top ", fixtureRanking, "highest_level", 100, true},
		{"power user", fixtureRanking, "normal", 100, true},
		{"unknown", fixtureRanking, "mapping_error", 0, false},
		{"User", `{"enabled":false}`, "not_configured", 0, false},
		{"User", "", "not_configured", 0, false},
	} {
		t.Run(test.level+test.state, func(t *testing.T) {
			rank, err := Rank(Snapshot{Level: test.level}, json.RawMessage(test.raw), time.Now())
			if err != nil || rank.StatusType != test.state || rank.OverallProgress != test.progress ||
				rank.CurrentIsKeep != test.keep || rank.Configured != (test.state != "not_configured") || rank.Requirements == nil || rank.LevelSteps == nil {
				t.Fatal(rank, err)
			}
			if rank.StatusType == "special_level" && (!rank.IsSpecialLevel || rank.SpecialLevelDescription != "永久会员") {
				t.Fatal(rank)
			}
		})
	}
}

func TestRankingValidatesRuleLinksAndFiniteValues(t *testing.T) {
	raw := json.RawMessage(`{"enabled":true,"rule_url":"javascript:alert(1)","ranking_system":{"levels":[{"name":"User"},{"name":"Next","order":1,"requirements":{"ratio":2,"bonus":"NaN","weeks":4}}]}}`)
	rank, err := Rank(Snapshot{Level: "User", Ratio: math.Inf(1), Bonus: math.NaN(), Joined: "2999-01-01"}, raw, time.Now())
	if err != nil || rank.RuleURL != "" || rank.Requirements[0].Current != 10000 ||
		!rank.Requirements[1].Met || rank.Requirements[1].Current != 0 || rank.Requirements[2].Current != 0 {
		t.Fatal(rank, err)
	}
	if _, err = Rank(Snapshot{}, json.RawMessage(`{"enabled":true,"ranking_system":[]}`), time.Now()); err == nil {
		t.Fatal("malformed rule became a valid mapping")
	}
}

func TestRankingComputesEveryBundledLevel(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range catalog.Indexers {
		if len(item.Ranking) == 0 {
			continue
		}
		var document struct {
			System struct {
				Levels  []rankLevel `json:"levels"`
				Special []rankLevel `json:"special_levels"`
			} `json:"ranking_system"`
		}
		if err := json.Unmarshal(item.Ranking, &document); err != nil {
			t.Fatal(item.ID, err)
		}
		for _, level := range append(document.System.Levels, document.System.Special...) {
			rank, err := Rank(Snapshot{Site: item.Name, Level: level.Name}, item.Ranking, time.Now())
			if err != nil || !rank.Configured || rank.StatusType == "mapping_error" {
				t.Fatal(item.ID, level.Name, rank, err)
			}
			if _, err = json.Marshal(rank); err != nil {
				t.Fatal(item.ID, level.Name, err)
			}
			count++
		}
	}
	if count < 100 {
		t.Fatal("missing original ranking levels", count)
	}
	t.Logf("evaluated %d original levels", count)
}
