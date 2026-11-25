package httpserver

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func TestTransferScheduleConsumesOnceAndKeepsPendingSlot(t *testing.T) {
	m := newMarkFixture(t)
	ctx := t.Context()
	ds, sys := m.api.downloaders, m.api.system
	dst, err := ds.Upsert(ctx, downloaderconfig.Downloader{Name: "Fixture target", Enabled: 1, Type: "transmission", Config: `{"host":"http://tr.fixture.test:9091"}`, DownloadDir: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.Set(ctx, "UserInstalledPlugins", `["TorrentTransfer"]`); err != nil {
		t.Fatal(err)
	}
	cfg := torrenttransfer.Settings{Enable: true, OnlyOnce: true, Cron: "0 * * * *", From: []string{strconv.FormatInt(m.item.ID, 10)}, To: []string{strconv.FormatInt(dst.ID, 10)}, TorrentPath: t.TempDir()}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.Set(ctx, torrenttransfer.ConfigKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/torrent-transfer/schedule"
	r := m.f.request(t, "GET", path, nil)
	var view struct {
		Result struct {
			Fingerprint string
			Control     transferSchedule
			Ready       bool
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &view) != nil || !view.Result.Ready {
		t.Fatal(r.Code, r.Body.String())
	}
	input := map[string]any{"epoch": view.Result.Control.Epoch, "fingerprint": view.Result.Fingerprint, "confirm": true}
	r = m.f.request(t, "POST", path+"/activate", input)
	var out struct{ Result transferSchedule }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil || out.Result.Once == nil || out.Result.Next == nil {
		t.Fatal(r.Code, r.Body.String())
	}
	api := torrentTransferAPI{system: sys, login: m.f.api, organizer: m.f.removal.organizer, downloaders: ds}
	at := out.Result.Once.Add(time.Minute)
	for range 2 {
		if err := api.queueDue(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	values, err := sys.ReadMany(ctx, transferScheduleKeys)
	if err != nil {
		t.Fatal(err)
	}
	st, err := parseTransferSchedule(values[transferScheduleKey])
	if err != nil || st.Pending == nil || st.Epoch != out.Result.Epoch {
		t.Fatal(st, err)
	}
	s, err := torrenttransfer.Parse(values[torrenttransfer.ConfigKey])
	if err != nil || s.OnlyOnce {
		t.Fatal(s, err)
	}
	r = m.f.request(t, "POST", path+"/stop", map[string]any{"confirm": true, "epoch": st.Epoch})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
