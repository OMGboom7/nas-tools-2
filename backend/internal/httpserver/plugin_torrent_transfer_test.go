package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func TestTransferPluginConfigPreservesExistingFieldsAndRejectsStaleSave(t *testing.T) {
	f := newLoginFixture(t, nil)
	store, err := systemconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := store.Set(ctx, torrenttransfer.ConfigKey, `{"notify":false,"unknown":9007199254740993}`); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/plugins/TorrentTransfer"
	if r := f.request(t, "POST", path+"/install", nil); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	r := f.request(t, "GET", path, nil)
	var config struct{ Data pluginConfigDetail }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &config) != nil || len(config.Data.Fields) != 13 || config.Data.Meta.Fingerprint == "" {
		t.Fatal(r.Code, r.Body.String())
	}
	save := map[string]any{"fingerprint": config.Data.Meta.Fingerprint, "values": map[string]bool{"notify": true}}
	if r := f.request(t, "PUT", path, save); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := f.request(t, "PUT", path, save); r.Code != 409 {
		t.Fatal("stale edit", r.Code)
	}
	if r := f.request(t, "DELETE", path, nil); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	values, err := store.ReadMany(ctx, []string{torrenttransfer.ConfigKey})
	if err != nil || !strings.Contains(values[torrenttransfer.ConfigKey], `"unknown":9007199254740993`) || !strings.Contains(values[torrenttransfer.ConfigKey], `"notify":true`) {
		t.Fatal(values, err)
	}
}
