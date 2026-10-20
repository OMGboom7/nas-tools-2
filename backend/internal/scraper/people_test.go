package scraper

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
)

func TestChinesePeoplePreferUniqueDoubanMatchAndUseVerifiedAliasesAsFallback(t *testing.T) {
	for _, mode := range []string{"douban", "fallback", "ambiguous", "wrong-person", "wrong-subject", "wrong-year", "substring"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := scraperClient(t)
			base := c.Transport
			personCalls := 0
			c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "frodo.douban.com" {
					if r.Header.Get("Authorization") != "" || r.URL.Query().Get("api_key") == c.Key {
						t.Fatal("TMDB key leaked to Douban")
					}
					switch r.URL.Path {
					case "/api/v2/search/movie":
						if mode == "fallback" || mode == "wrong-person" {
							return response(r, `{"items":[]}`), nil
						}
						if mode == "wrong-year" {
							return response(r, `{"items":[{"target_id":"300","target_type":"movie","target":{"title":"Movie <&>","year":"1999"}}]}`), nil
						}
						if mode == "ambiguous" {
							return response(r, `{"items":[{"target_id":"300","target_type":"movie","target":{"title":"Movie <&>","year":"2026"}},{"target_id":"301","target_type":"movie","target":{"title":"Movie <&>","year":"2026"}}]}`), nil
						}
						return response(r, `{"items":[{"target_id":"300","target_type":"movie","target":{"title":"Movie <&>","year":"2026"}}]}`), nil
					case "/api/v2/movie/300":
						if mode == "wrong-subject" {
							return response(r, `{"id":"301","title":"Movie <&>","year":"2026"}`), nil
						}
						return response(r, `{"id":"300","title":"Movie <&>","year":"2026","imdb":"tt123"}`), nil
					case "/api/v2/movie/300/celebrities":
						name := "Smith John"
						if mode == "substring" {
							name = "Smith Johnny"
						}
						return response(r, `{"directors":[{"id":"400","name":"陳 導 演","latin_name":"Director"}],"actors":[{"id":"401","name":"張 三","latin_name":"`+name+`","character":"饰 王爺"}]}`), nil
					}
					t.Fatal("wrong Douban scope", r.URL)
				}
				if strings.HasPrefix(r.URL.Path, "/3/person/") {
					personCalls++
					id := strings.TrimPrefix(r.URL.Path, "/3/person/")
					if mode == "wrong-person" {
						return response(r, `{"id":999,"also_known_as":["錯誤中文名"]}`), nil
					}
					name := "李 曉 華"
					if id == "3" {
						name = "陳導演"
					}
					return response(r, `{"id":`+id+`,"also_known_as":["`+name+`"]}`), nil
				}
				if r.URL.Path == "/3/movie/100" {
					body := strings.Replace(movie, `"Actor"`, `"John Smith"`, 1)
					return response(r, body), nil
				}
				return base.RoundTrip(r)
			})
			o, _, _ := Parse(`{"scraper_nfo":{"movie":{"basic":true,"credits":true,"credits_chinese":true}}}`)
			p, err := c.Build(t.Context(), Input{Kind: "movie", ID: "100", Stem: "Movie"}, o, time.Now())
			if err != nil || len(p.Artifacts) != 1 {
				t.Fatal(p, err)
			}
			body := string(p.Artifacts[0].Data)
			if mode == "douban" {
				if !strings.Contains(body, "<name>张三</name>") || !strings.Contains(body, "<role>王爺</role>") || !strings.Contains(body, "陈导演</director>") || personCalls != 0 || len(p.Warnings) != 0 {
					t.Fatal(body, p.Warnings, personCalls)
				}
			} else if mode == "wrong-person" {
				if strings.Contains(body, "错误中文名") || !strings.Contains(body, "<name>John Smith</name>") || len(p.Warnings) == 0 {
					t.Fatal(body, p.Warnings)
				}
			} else {
				if strings.Contains(body, "<name>张三</name>") || !strings.Contains(body, "<name>李晓华</name>") {
					t.Fatal("unsafe or missing fallback", body)
				}
			}
			if !strings.Contains(body, `<tmdbid>2</tmdbid>`) || !strings.Contains(body, "<order>0</order>") {
				t.Fatal("identity/order changed", body)
			}
		})
	}
}
func TestWholePersonNameMatchingNeverUsesSubstringOrAmbiguousRows(t *testing.T) {
	p := []douban.Celebrity{{ID: json.RawMessage(`"1"`), Name: "張三", Latin: "John Smith"}, {ID: json.RawMessage(`"2"`), Name: "李四", Latin: "Smith John"}}
	if _, ok := matchCelebrity([]string{"John Smith"}, p); ok {
		t.Fatal("ambiguous names guessed")
	}
	if _, ok := matchCelebrity([]string{"John Smithson"}, p[:1]); ok {
		t.Fatal("substring name matched")
	}
	if _, ok := matchCelebrity([]string{"Smith John"}, p[:1]); !ok {
		t.Fatal("reversed exact name not matched")
	}
	if out, ok := chineseName("陳 曉 華"); !ok || out != "陈晓华" {
		t.Fatal(out, ok)
	}
}
