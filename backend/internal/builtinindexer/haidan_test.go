package builtinindexer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestHaiDanUsesGroupAndIndividualTorrentTitles(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	var definition indexercatalog.Definition
	for _, item := range catalog.Indexers {
		if item.ID == "haidan" {
			definition = item
		}
	}
	plan, err := Build(definition, "Movie & 中文", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.URL.Query().Get("search") != "Movie & 中文" {
		t.Fatal(plan.URL)
	}
	body := `<div class="torrent_panel_inner"><div class="torrent_group"><a href="details.php?group_id=5">第一组</a>
		<div class="torrent_wrap"><a href="details.php?group_id=5&amp;torrent_id=10">1080p.WEB-DL</a><a href="download.php?id=10">download</a><div class="video_size">1 GB</div><div class="seeder_col">3</div><img class="pro_free"></div>
		<div class="torrent_wrap"><a href="details.php?group_id=5&amp;torrent_id=11">2160p.BluRay</a><a href="download.php?id=11">download</a><div class="video_size">2 GB</div><div class="seeder_col">4</div><img class="pro_50pctdown"></div>
	</div><div class="torrent_group"><a href="details.php?group_id=6">第二组</a><div class="torrent_wrap"><a href="details.php?group_id=6&amp;torrent_id=12">720p</a><a href="download.php?id=12">download</a><div class="video_size">1 GB</div></div></div></div>`
	options := ResultOptions{Now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), Limit: 100}
	resources, err := ParseResults(context.Background(), plan, []byte(body), options)
	if err != nil || len(resources) != 3 {
		t.Fatal(resources, err)
	}
	for index, want := range []string{"第一组 1080p.WEB-DL", "第一组 2160p.BluRay", "第二组 720p"} {
		if resources[index].Title != want {
			t.Fatal(resources[index].Title, want)
		}
	}
	if resources[0].PageURL != "https://www.haidan.video/details.php?group_id=5&torrent_id=10" || resources[0].DownloadFactor == nil || *resources[0].DownloadFactor != 0 || resources[1].DownloadFactor == nil || *resources[1].DownloadFactor != 0.5 {
		t.Fatal(resources)
	}
	options.Limit = 1
	resources, err = ParseResults(context.Background(), plan, []byte(body), options)
	if err != nil || len(resources) != 1 {
		t.Fatal(resources, err)
	}
	var rules torrentRules
	if json.Unmarshal(definition.Torrents, &rules) != nil {
		t.Fatal("invalid catalog")
	}
	normalizeHaiDanID(rules.Fields)
	doc, err := ParseDocument(context.Background(), []byte(`<div><a href="details.php?group_id=5&amp;torrent_id=10">title</a></div>`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := FieldValue(context.Background(), doc.Root, rules.Fields["id"])
	if err != nil || id != "10" {
		t.Fatal(id, err)
	}
}
