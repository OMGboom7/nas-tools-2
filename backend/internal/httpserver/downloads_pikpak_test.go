package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestPikPakNativeTasksAndServiceChecksNeverUseLegacy(t *testing.T) {
	reads, signins, checks := 0, 0, 0
	d := downloaderconfig.Downloader{Name: "Cloud", Type: "pikpak", Enabled: 1, Config: `{"username":"private-user","password":"private-password"}`}
	h, token, _ := nativeServicesFixture(t, "", &d, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		checks++
		switch r.URL.Host + r.URL.Path {
		case "user.mypikpak.com/v1/shield/captcha/init":
			return jsonResponse(r, `{"captcha_token":"private-challenge"}`), nil
		case "user.mypikpak.com/v1/auth/signin":
			signins++
			return jsonResponse(r, `{"access_token":"private-token","sub":"User_A"}`), nil
		case "api-drive.mypikpak.com/drive/v1/tasks":
			reads++
			return jsonResponse(r, `{"tasks":[{"id":"Task_A","file_name":"Movie.mkv","phase":"PHASE_TYPE_RUNNING"},{"id":"Task_B","file_name":"Failed.mkv","phase":"PHASE_TYPE_ERROR"}]}`), nil
		default:
			t.Errorf("unexpected legacy/mutation %s", r.URL)
			return nil, fmt.Errorf("forbidden")
		}
	}))
	w := nativeJSONRequest(h, "GET", "/api/v1/downloads", token, "")
	var result struct {
		Data downloadsData `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || reads != 1 || len(result.Data.Active) != 2 {
		t.Fatal(w.Code, w.Body.String(), reads)
	}
	for _, row := range result.Data.Active {
		if row.CanControl || row.CanRemove || row.ShowProgress {
			t.Fatal("invented unsupported control/progress", row)
		}
	}
	if result.Data.Active[0].ID != "Task_A" || result.Data.Active[1].State != "Error" {
		t.Fatal(result.Data.Active)
	}
	for _, secret := range []string{"private-user", "private-password", "private-token", "private-challenge"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leak")
		}
	}
	w = nativeJSONRequest(h, "POST", fmt.Sprintf("/api/v1/services/downloader/%d/test", d.ID), token, `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || signins != 2 || checks != 5 {
		t.Fatal(w.Code, w.Body.String(), signins, checks)
	}
}

func TestPikPakManualReceiptPreservesCaseAndLostResponsesNeverReplay(t *testing.T) {
	f := manualHTTPFixture(t, "pikpak", false)
	if w := f.magnet(); w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	rows := manualReceipts(t, f)
	if len(rows) != 1 || rows[0].State != "submitted" || rows[0].TaskID != "Task_CaseSensitive" {
		t.Fatal(rows)
	}
	var id string
	if err := f.db.QueryRow(`SELECT DOWNLOAD_ID FROM DOWNLOAD_HISTORY`).Scan(&id); err != nil || id != "Task_CaseSensitive" {
		t.Fatal(id, err)
	}
	f.rebuild(t)
	if w := f.magnet(); w.Code != 200 || f.adds != 1 || !strings.Contains(w.Body.String(), "Task_CaseSensitive") {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	f2 := manualHTTPFixture(t, "pikpak", false)
	f2.lost = true
	if w := f2.magnet(); w.Code != 409 || f2.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f2.adds)
	}
	row := manualReceipts(t, f2)[0]
	if row.State != "pending" {
		t.Fatal(row)
	}
	f2.rebuild(t)
	if w := f2.magnet(); w.Code != 409 || f2.adds != 1 {
		t.Fatal("replayed", w.Code, f2.adds)
	}
	if w := manualVerify(f2, row.ID); w.Code != 501 || f2.adds != 1 {
		t.Fatal("unproven reconciliation", w.Code, w.Body.String())
	}
}

func TestPikPakRejectsUnimplementedSettingsBeforeNetwork(t *testing.T) {
	f := manualHTTPFixture(t, "pikpak", false)
	item, err := f.store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	s := downloadService{downloaders: f.store, systemConfig: system, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("unsupported option used network", r.URL)
		return nil, nil
	})}}
	for _, o := range []downloadAddOptions{{SavePath: "/NAS/path"}, {Tags: []string{"site"}}, {Paused: true}, {RatioLimit: 1}, {UploadLimitKB: 1}, {ApplyTransmissionLimits: true}} {
		if searchDownloadOptionsSupported("pikpak", o) || rssOptionsSupported("pikpak", o) {
			t.Fatal("unsupported options accepted", o)
		}
		if _, handled, err := s.nativeAddConfiguredDownload(t.Context(), item, testMagnet, nil, o); !handled || err == nil {
			t.Fatal(handled, err)
		}
	}
	if !searchDownloadOptionsSupported("pikpak", downloadAddOptions{SavePath: "Folder_A"}) {
		t.Fatal("valid cloud folder rejected")
	}
	if _, handled, err := s.nativeAddConfiguredDownload(t.Context(), item, "", testTorrent, downloadAddOptions{}); !handled || err == nil {
		t.Fatal("torrent uploaded", handled, err)
	}
	for _, action := range []string{"start", "stop", "remove"} {
		if handled, err := s.nativeControl(t.Context(), "Task_A", action); !handled || err != errDownloaderControlUnsupported {
			t.Fatal(action, handled, err)
		}
	}
}

func TestPikPakSigninFailureBeforeSubmissionAllowsExplicitNewAttempt(t *testing.T) {
	f := manualHTTPFixture(t, "pikpak", false)
	f.authFailure = true
	if w := f.magnet(); w.Code == 200 || f.adds != 0 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	rows := manualReceipts(t, f)
	if len(rows) != 1 || rows[0].State != "rejected" {
		t.Fatal("pre-submit failure left ambiguous receipt", rows)
	}
	f.authFailure = false
	if w := f.magnet(); w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	assertManualCommitted(t, f, 1)
}
