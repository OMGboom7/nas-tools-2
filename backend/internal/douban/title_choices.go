package douban

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type TitleChoice struct {
	ID       json.RawMessage `json:"id"`
	Kind     string          `json:"kind"`
	Title    string          `json:"title"`
	Year     json.RawMessage `json:"year"`
	Overview string          `json:"overview"`
	Image    string          `json:"image"`
	Vote     json.RawMessage `json:"vote"`
}

var doubanMobileImageHost = regexp.MustCompile(`^qnmob[0-9]+\.`)

// TitleChoices uses the same mixed catalog as the chat and web title pickers.
// Subject matching elsewhere uses /search/movie and has different completeness requirements.
func (c Client) TitleChoices(ctx context.Context, keyword, kind string, page int) ([]TitleChoice, error) {
	if strings.TrimSpace(keyword) == "" || page < 1 || page > 500 {
		return nil, ErrResponse
	}
	var result struct {
		Items []struct {
			TypeName string `json:"type_name"`
			Target   struct {
				ID       json.RawMessage `json:"id"`
				Title    string          `json:"title"`
				Year     json.RawMessage `json:"year"`
				Cover    string          `json:"cover_url"`
				Subtitle string          `json:"card_subtitle"`
				Rating   struct {
					Value json.RawMessage `json:"value"`
				} `json:"rating"`
			} `json:"target"`
		} `json:"items"`
	}
	query := url.Values{"q": {keyword}, "start": {strconv.Itoa((page - 1) * 20)}, "count": {"20"}}
	if c.get(ctx, "/search/weixin", query, &result) != nil || result.Items == nil || len(result.Items) > 20 {
		return nil, ErrResponse
	}
	rows := []TitleChoice{}
	for _, entry := range result.Items {
		var mediaType string
		switch entry.TypeName {
		case "电影":
			mediaType = "movie"
		case "电视剧":
			mediaType = "tv"
		default:
			continue
		}
		item := entry.Target
		if (kind != "" && mediaType != kind) || ID(item.ID) == "" || strings.TrimSpace(item.Title) == "" {
			continue
		}
		cover, _, _ := strings.Cut(item.Cover, "?")
		if u, err := url.Parse(cover); err == nil && u.Host != "" {
			u.Host = doubanMobileImageHost.ReplaceAllString(u.Host, "img1.")
			cover = u.String()
		}
		rows = append(rows, TitleChoice{ID: item.ID, Kind: mediaType, Title: item.Title, Year: item.Year, Overview: item.Subtitle, Image: cover, Vote: item.Rating.Value})
	}
	return rows, nil
}
