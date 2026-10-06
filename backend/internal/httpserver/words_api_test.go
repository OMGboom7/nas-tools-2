package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestCustomWordsNativeLifecycleAndAtomicEdits(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	post := func(action string, form url.Values, status int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/words/"+action, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s=%d %s", action, response.Code, response.Body.String())
		}
		return response
	}
	form := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"2"}, "enabled": {"1"}, "regex": {"1"}, "new_replaced": {"Original"}, "new_replace": {"Corrected"}}
	post("item/update", form, 200)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`UPDATE CUSTOM_WORDS SET NOTE='preserve-note' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	form.Set("id", "1")
	form.Set("new_replace", "Updated")
	post("item/update", form, 200)
	var note, replacement string
	var season int
	if err := database.QueryRow(`SELECT NOTE,"REPLACE",SEASON FROM CUSTOM_WORDS WHERE ID=1`).Scan(&note, &replacement, &season); err != nil || note != "preserve-note" || replacement != "Updated" || season != -2 {
		t.Fatalf("edited word note=%q replace=%q season=%d err=%v", note, replacement, season, err)
	}
	response := post("item/info", url.Values{"wid": {"1"}}, 200)
	if !strings.Contains(response.Body.String(), `"replace":"Updated"`) {
		t.Fatal(response.Body.String())
	}
	form.Set("id", "0")
	response = post("item/update", form, 200)
	if !strings.Contains(response.Body.String(), `"code":1`) {
		t.Fatalf("duplicate=%s", response.Body.String())
	}
	form.Set("new_replaced", "Other")
	post("item/update", form, 200)
	form.Set("id", "1")
	response = post("item/update", form, 200)
	if !strings.Contains(response.Body.String(), `"code":1`) {
		t.Fatalf("conflicting edit=%s", response.Body.String())
	}
	var original string
	if err := database.QueryRow(`SELECT REPLACED FROM CUSTOM_WORDS WHERE ID=1`).Scan(&original); err != nil || original != "Original" {
		t.Fatalf("conflicting edit lost original=%q err=%v", original, err)
	}
	post("item/status", url.Values{"flag": {"0"}, "ids_info": {`["word_1"]`}}, 200)
	var enabled int
	if err := database.QueryRow(`SELECT ENABLED FROM CUSTOM_WORDS WHERE ID=1`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("status=%d %v", enabled, err)
	}
	response = post("list", nil, 200)
	var result struct {
		Result []map[string]any `json:"result"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Result) != 1 || result.Result[0]["name"] != "通用" {
		t.Fatalf("list=%s", response.Body.String())
	}
	post("item/delete", url.Values{"id": {"1"}}, 200)
	response = post("item/info", url.Values{"wid": {"1"}}, 200)
	if !strings.Contains(response.Body.String(), `"data":{}`) {
		t.Fatalf("deleted info=%s", response.Body.String())
	}
}

func TestCustomWordsStatusBatchRollsBackAndRejectsViewer(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`INSERT INTO CUSTOM_WORDS (ID,REPLACED,ENABLED) VALUES (1,'First',1),(2,'Second',1)`,
		`CREATE TRIGGER reject_second_status BEFORE UPDATE ON CUSTOM_WORDS WHEN OLD.ID=2 BEGIN SELECT RAISE(ABORT,'test failure'); END`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	form := url.Values{"flag": {"disable"}, "ids_info": {`["word_1","word_2"]`}}
	response := performFormRequest(handler, "/api/v1/words/item/status", token, form)
	if response.Code != 502 {
		t.Fatalf("batch=%d %s", response.Code, response.Body.String())
	}
	var enabled int
	if err := database.QueryRow(`SELECT ENABLED FROM CUSTOM_WORDS WHERE ID=1`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("batch rollback enabled=%d err=%v", enabled, err)
	}
	response = performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	response = performFormRequest(handler, "/api/v1/words/item/delete", viewer, url.Values{"id": {"1"}})
	if response.Code != 403 {
		t.Fatalf("viewer mutation=%d %s", response.Code, response.Body.String())
	}
}

func TestCustomWordsGroupDeleteRollsBackAndRequiresLogin(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`INSERT INTO CUSTOM_WORD_GROUPS (ID,TITLE,TYPE) VALUES (1,'Group',2)`,
		`INSERT INTO CUSTOM_WORDS (ID,REPLACED,GROUP_ID) VALUES (1,'Keep',1)`,
		`CREATE TRIGGER reject_word_group_delete BEFORE DELETE ON CUSTOM_WORD_GROUPS BEGIN SELECT RAISE(ABORT,'test failure'); END`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, auth := range []string{"", "bad", token} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/words/group/delete", strings.NewReader("gid=1"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", auth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := 401
		if auth == token {
			want = 502
		}
		if response.Code != want {
			t.Fatalf("delete=%d %s", response.Code, response.Body.String())
		}
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM CUSTOM_WORDS WHERE ID=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("word was not rolled back: %d %v", count, err)
	}
}

func TestCustomWordGroupsCreatedFromNativeTMDB(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "tmdb.test" || request.URL.Query().Get("api_key") != "server-key" {
			t.Fatalf("unexpected group request: %s", request.URL)
		}
		switch request.URL.Path {
		case "/3/movie/200":
			return jsonResponse(request, `{"id":200,"title":"电影词组","release_date":"2024-03-01"}`), nil
		case "/3/tv/200":
			return jsonResponse(request, `{"id":200,"name":"剧集词组","first_air_date":"2025-01-01","number_of_seasons":3}`), nil
		case "/3/tv/300":
			result := jsonResponse(request, `{}`)
			result.StatusCode = http.StatusFound
			result.Header.Set("Location", "http://legacy:3000/api/v1/words/group/add")
			return result, nil
		default:
			t.Fatalf("unexpected group path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"movie", "tv"} {
		response := performFormRequest(handler, "/api/v1/words/group/add", token, url.Values{"tmdb_id": {"200"}, "tmdb_type": {kind}})
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"code":0`) {
			t.Fatalf("add %s=%d %s", kind, response.Code, response.Body.String())
		}
	}
	response := performFormRequest(handler, "/api/v1/words/group/add", token, url.Values{"tmdb_id": {"200"}, "tmdb_type": {"tv"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"code":1`) || requests != 2 {
		t.Fatalf("duplicate=%d %s requests=%d", response.Code, response.Body.String(), requests)
	}
	response = performFormRequest(handler, "/api/v1/words/group/add", token, url.Values{"tmdb_id": {"300"}, "tmdb_type": {"tv"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"code":1`) {
		t.Fatalf("redirect=%d %s", response.Code, response.Body.String())
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var title, year string
	var seasons, count int
	if err := database.QueryRow(`SELECT TITLE,YEAR,SEASON_COUNT FROM CUSTOM_WORD_GROUPS WHERE TYPE=2`).Scan(&title, &year, &seasons); err != nil || title != "剧集词组" || year != "2025" || seasons != 3 {
		t.Fatalf("TV group=%q %q %d err=%v", title, year, seasons, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM CUSTOM_WORD_GROUPS`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unexpected group count=%d err=%v", count, err)
	}
}
