package pan115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestTasksPaginatesAndFiltersActive(t *testing.T) {
	const first = "0123456789abcdef0123456789abcdef01234567"
	const second = "abcdef0123456789abcdef0123456789abcdef01"
	pages := []string{}
	client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://115.com/web/lixian/?ct=lixian&ac=task_lists" || request.Header.Get("Cookie") != "UID=private-cookie" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected request: %s %s %v", request.Method, request.URL, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		page := values.Get("page")
		pages = append(pages, page)
		switch page {
		case "1":
			return response(200, `{"state":true,"count":4,"page_count":2,"tasks":[{"info_hash":"`+first+`","name":"First movie","status":0,"percentDone":42.5},{"info_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Done","status":2,"percentDone":100}]}`), nil
		case "2":
			return response(200, `{"state":true,"count":4,"page_count":2,"tasks":[{"info_hash":"`+first+`","name":"Duplicate","status":1,"percentDone":43},{"info_hash":"`+second+`","name":"Second movie","status":1,"percentDone":"75.5"}]}`), nil
		default:
			t.Fatalf("unexpected page %q", page)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 2 || tasks[0] != (Task{first, "First movie", 42.5}) || tasks[1] != (Task{second, "Second movie", 75.5}) || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("tasks=%+v err=%v pages=%v", tasks, err, pages)
	}
}

func TestTasksRejectsFailuresWithoutFollowingRedirects(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, item := range []struct {
		status int
		body   string
		want   error
	}{
		{401, "UID=private-cookie", ErrAuthentication},
		{302, "UID=private-cookie", ErrResponse},
		{200, `{"state":false,"error":"UID=private-cookie"}`, ErrAuthentication},
		{200, `{"state":true,"count":1,"page_count":101,"tasks":[]}`, ErrResponse},
		{200, `{"state":true,"count":1,"page_count":1,"tasks":[{"info_hash":"bad","name":"Movie","status":0,"percentDone":10}]}`, ErrResponse},
		{200, `{"state":true,"count":1,"page_count":1,"tasks":[{"info_hash":"` + hash + `","name":"Movie","status":0,"percentDone":101}]}`, ErrResponse},
		{200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		calls := 0
		client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Host != "115.com" {
				t.Fatalf("followed redirect: %s", request.URL)
			}
			result := response(item.status, item.body)
			result.Header.Set("Location", "https://other.example/collect")
			return result, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, taskErr := client.Tasks(context.Background())
		if !errors.Is(taskErr, item.want) || strings.Contains(taskErr.Error(), "private-cookie") || calls != 1 {
			t.Fatalf("tasks error=%v calls=%d", taskErr, calls)
		}
	}
}

func TestDeleteUsesOneValidatedHashAndNoRedirect(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	calls := 0
	client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.String() != "https://115.com/web/lixian/?ct=lixian&ac=task_del" || request.Header.Get("Cookie") != "UID=private-cookie" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected delete request: %s %s %v", request.Method, request.URL, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		values, err := url.ParseQuery(string(body))
		if err != nil || len(values) != 1 || values.Get("hash[0]") != hash {
			t.Fatalf("unexpected delete form: %s err=%v", body, err)
		}
		return response(200, `{"state":true}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), "all"); !errors.Is(err, ErrConfiguration) || calls != 0 {
		t.Fatalf("invalid delete err=%v calls=%d", err, calls)
	}
	if err := client.Delete(context.Background(), hash); err != nil || calls != 1 {
		t.Fatalf("delete err=%v calls=%d", err, calls)
	}
	for _, item := range []struct {
		status int
		body   string
		want   error
	}{
		{401, "UID=private-cookie", ErrAuthentication},
		{302, "UID=private-cookie", ErrResponse},
		{200, `{"state":false,"error_msg":"UID=private-cookie"}`, ErrResponse},
		{200, `<html>UID=private-cookie</html>`, ErrResponse},
		{200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		redirectCalls := 0
		failed, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			redirectCalls++
			if request.URL.Host != "115.com" {
				t.Fatalf("followed redirect: %s", request.URL)
			}
			result := response(item.status, item.body)
			result.Header.Set("Location", "https://other.example/collect")
			return result, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		deleteErr := failed.Delete(context.Background(), hash)
		if !errors.Is(deleteErr, item.want) || strings.Contains(deleteErr.Error(), "private-cookie") || redirectCalls != 1 {
			t.Fatalf("delete err=%v calls=%d", deleteErr, redirectCalls)
		}
	}
}

func TestAddMagnetResolvesRootAndSubmitsOneTask(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	const magnet = "magnet:?xt=urn:btih:" + hash + "&dn=Movie"
	calls := 0
	client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Cookie") != "UID=private-cookie" {
			t.Fatalf("cookie missing for %s", request.URL)
		}
		switch calls {
		case 1:
			if request.Method != http.MethodGet || request.URL.String() != "https://webapi.115.com/files/getid?path=%2F" {
				t.Fatalf("directory request: %s %s", request.Method, request.URL)
			}
			return response(200, `{"state":true,"id":0}`), nil
		case 2:
			if request.Method != http.MethodPost || request.URL.String() != "https://115.com/web/lixian/?ct=lixian&ac=add_task_urls" {
				t.Fatalf("add request: %s %s", request.Method, request.URL)
			}
			body, _ := io.ReadAll(request.Body)
			values, err := url.ParseQuery(string(body))
			if err != nil || len(values) != 3 || values.Get("url[0]") != magnet || values.Get("wp_path_id") != "0" || values.Get("savepath") != "" {
				t.Fatalf("add form: %s err=%v", body, err)
			}
			return response(200, `{"state":true,"result":[{"info_hash":"`+hash+`"}]}`), nil
		default:
			t.Fatalf("unexpected call %d", calls)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.AddMagnet(context.Background(), magnet)
	if err != nil || id != hash || calls != 2 {
		t.Fatalf("add id=%q err=%v calls=%d", id, err, calls)
	}
}

func TestAddMagnetFailsClosedBeforeTaskSubmission(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	const magnet = "magnet:?xt=urn:btih:" + hash
	for _, item := range []struct {
		status int
		body   string
		want   error
	}{
		{401, "UID=private-cookie", ErrAuthentication},
		{302, "UID=private-cookie", ErrResponse},
		{200, `{"state":false,"error":"UID=private-cookie"}`, ErrResponse},
		{200, `{"state":true,"id":"bad"}`, ErrResponse},
		{200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		calls := 0
		client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Host != "webapi.115.com" {
				t.Fatalf("unsafe redirect or task submitted: %s", request.URL)
			}
			result := response(item.status, item.body)
			result.Header.Set("Location", "https://other.example/collect")
			return result, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, addErr := client.AddMagnet(context.Background(), magnet)
		if !errors.Is(addErr, item.want) || strings.Contains(addErr.Error(), "private-cookie") || calls != 1 {
			t.Fatalf("add err=%v calls=%d", addErr, calls)
		}
	}
}

func TestAddMagnetRejectsFailedTaskResponse(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, item := range []struct {
		status int
		body   string
		want   error
	}{
		{403, "UID=private-cookie", ErrAuthentication},
		{302, "UID=private-cookie", ErrResponse},
		{200, `{"state":false,"error":"UID=private-cookie"}`, ErrResponse},
		{200, `{"state":true,"result":[{"info_hash":"bad"}]}`, ErrResponse},
		{200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		calls := 0
		client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response(200, `{"state":true,"id":0}`), nil
			}
			if calls != 2 || request.URL.Host != "115.com" {
				t.Fatalf("followed redirect: %s calls=%d", request.URL, calls)
			}
			result := response(item.status, item.body)
			result.Header.Set("Location", "https://other.example/collect")
			return result, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, addErr := client.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+hash)
		if !errors.Is(addErr, item.want) || strings.Contains(addErr.Error(), "private-cookie") || calls != 2 {
			t.Fatalf("add err=%v calls=%d", addErr, calls)
		}
	}
}

func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCheckUsesCookieOnlyFor115AccountProbe(t *testing.T) {
	calls := 0
	client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.String() != "https://webapi.115.com/files/index_info" || request.Header.Get("Cookie") != "UID=private-cookie" {
			t.Fatalf("unexpected request: %s %s %v", request.Method, request.URL, request.Header)
		}
		return response(200, `{"state":true,"data":{"space_info":{"all_total":{"size_format":"1 TB"}}}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil || calls != 1 {
		t.Fatalf("check = %v calls=%d", err, calls)
	}
}

func TestCheckRejectsFailureAndRedirectWithoutLeakingCookie(t *testing.T) {
	for _, item := range []struct {
		status int
		body   string
		want   error
	}{
		{200, `{"state":false,"data":{}}`, ErrAuthentication},
		{401, "UID=private-cookie", ErrAuthentication},
		{302, "UID=private-cookie", ErrResponse},
		{200, "<html>UID=private-cookie</html>", ErrResponse},
		{200, `{"state":true}`, ErrResponse},
		{200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		calls := 0
		client, err := New("UID=private-cookie", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Host != "webapi.115.com" {
				t.Fatalf("followed redirect: %s", request.URL)
			}
			result := response(item.status, item.body)
			result.Header.Set("Location", "https://other.example/collect")
			return result, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		checkErr := client.Check(context.Background())
		if !errors.Is(checkErr, item.want) || strings.Contains(checkErr.Error(), "private-cookie") || calls != 1 {
			t.Fatalf("check = %v calls=%d", checkErr, calls)
		}
	}
	for _, cookie := range []string{"", "UID=good\r\nX-Evil: value"} {
		if _, err := New(cookie, nil); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted cookie %q", cookie)
		}
	}
}
