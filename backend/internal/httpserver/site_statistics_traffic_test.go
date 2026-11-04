package httpserver

import (
	"database/sql"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSiteStatisticsTrafficKeepsOriginalDatesScopeAndBytePrecision(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	f, api := newStatsFixture(t)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO CONFIG_SITE(ID,NAME,PRI,SIGNURL,COOKIE,INCLUDE) VALUES
 (9007199254740993,'Fixture site','2','https://other.fixture.test/','fixture-cookie','T'),
 (9007199254740995,'Excluded site','3','https://hidden.fixture.test/','fixture-cookie','');
INSERT INTO SITE_STATISTICS_HISTORY(SITE,DATE,UPLOAD,DOWNLOAD,URL) VALUES
 ('Old name','2025-12-31','10','10','https://site.fixture.test'),
 ('Old name','2026-01-01','9007199254741003','12','https://site.fixture.test'),
 ('Fixture site','2026-01-02','9007199254741023','20','https://site.fixture.test'),
 ('Fixture site','2025-12-31','1000','2000','https://other.fixture.test'),
 ('Fixture site','2026-01-01','1010','2020','https://other.fixture.test'),
 ('Excluded site','2026-01-01','100','100','https://hidden.fixture.test');`); err != nil {
		t.Fatal(err)
	}
	var upstream atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return nil, http.ErrNotSupported
	})
	if _, err := f.api.auth.service.CreateUser(t.Context(), "traffic-viewer", "fixture-password", nil); err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, f.handler, "traffic-viewer", "fixture-password")
	const path = "/api/v1/site-statistics/traffic"
	w := nativeJSONRequest(f.handler, "GET", path+"?days=1&endDay=2026-01-01", token, "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"endDay":"2026-01-01"`) ||
		!strings.Contains(body, `"upload":"9007199254740993"`) || !strings.Contains(body, `"upload":"10"`) ||
		!strings.Contains(body, `"siteId":"9007199254740993"`) || strings.Contains(body, "Excluded") ||
		strings.Contains(body, "Old name") || strings.Contains(body, "fixture-cookie") {
		t.Fatal(w.Code, body)
	}
	w = nativeJSONRequest(f.handler, "GET", path+"?days=7&endDay=2026-01-02", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"upload":"9007199254741013"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "GET", path+"?days=365&endDay=2020-01-01", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, query := range []string{"?days=0", "?days=2", "?days=1&endDay=2026-2-01", "?days=30&endDay=9999-12-31"} {
		if w := nativeJSONRequest(f.handler, "GET", path+query, token, ""); w.Code != 400 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	if w := nativeJSONRequest(f.handler, "GET", path+"?days=1", "", ""); w.Code != 401 {
		t.Fatal("anonymous traffic request", w.Code)
	}
	jobs, err := api.store.Jobs(t.Context())
	if err != nil || len(jobs) != 0 || upstream.Load() != 0 {
		t.Fatal("traffic read created external work", jobs, err, upstream.Load())
	}
}
