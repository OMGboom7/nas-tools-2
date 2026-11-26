package httpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

type titleQuery struct {
	Title string
	Kind  string
	Year  string
	Page  int
}

type tmdbTitleChoice struct {
	Kind string          `json:"kind"`
	Info json.RawMessage `json:"info"`
	date string
}

func SearchTitlesCommand(ctx context.Context, configPath, provider string, in io.Reader, out io.Writer) error {
	if provider == "recognize" {
		return recognitionLookupCommand(ctx, configPath, in, out)
	}
	if provider == "subject" {
		return sourceSubjectCommand(ctx, in, out)
	}
	if provider == "match" {
		return titleMatchCommand(ctx, configPath, in, out)
	}
	if provider == "criteria" {
		return chatTitleCriteria(in, out)
	}
	if provider == "start" || provider == "choices" {
		return chatChoiceCommand(ctx, configPath, provider, in, out)
	}
	input := &io.LimitedReader{R: in, N: 64 << 10}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	var q titleQuery
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || strings.TrimSpace(q.Title) == "" || q.Page < 1 || q.Page > 500 || (q.Kind != "" && q.Kind != "movie" && q.Kind != "tv") {
		return errors.New("invalid media title query")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	switch provider {
	case "tmdb":
		snapshot, err := config.NewStore(configPath).Snapshot()
		if err != nil {
			return err
		}
		client := &http.Client{}
		proxies := objectValue(objectValue(snapshot["app"])["proxies"])
		if text(proxies["https"]) != "" {
			client.Transport, err = siteProxyTransport(nil, proxies, "https")
			if err != nil {
				return err
			}
			defer client.CloseIdleConnections()
		}
		rows, err := searchTMDBTitles(ctx, client, snapshot, q)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(rows)
	case "douban":
		rows, err := (douban.Client{}).TitleChoices(ctx, q.Title, q.Kind, q.Page)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(rows)
	}
	return errors.New("unknown media title source")
}

func searchTMDBTitles(ctx context.Context, client *http.Client, snapshot map[string]any, q titleQuery) ([]tmdbTitleChoice, error) {
	language := "en"
	if strings.ContainsFunc(q.Title, func(r rune) bool { return r >= '一' && r <= '\u9fff' }) {
		language = "zh"
	}
	kinds, upstreamPage := []string{q.Kind}, q.Page
	merge := q.Kind == "" && q.Year != ""
	if merge {
		// The existing dated search merges the first movie and TV batches,
		// sorts by release date, then paginates that combined list.
		kinds, upstreamPage = []string{"movie", "tv"}, 1
	} else if q.Kind == "" {
		kinds = []string{"multi"}
	}
	out := []tmdbTitleChoice{}
	for _, kind := range kinds {
		filters := map[string]string{"language": language, "query": q.Title}
		if q.Year != "" {
			if kind == "movie" {
				filters["year"] = q.Year
			} else {
				filters["first_air_date_year"] = q.Year
			}
		}
		page, err := fetchTMDBResultPage[json.RawMessage](ctx, client, snapshot, "/search/"+kind, upstreamPage, filters)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Results {
			var info tmdbRecommendation
			if err := json.Unmarshal(item, &info); err != nil {
				return nil, err
			}
			mediaType := kind
			if kind == "multi" {
				mediaType = info.MediaType
			}
			if info.ID <= 0 || (mediaType != "movie" && mediaType != "tv") || kind == "tv" && !strings.Contains(info.Name, q.Title) {
				continue
			}
			date := info.ReleaseDate
			if mediaType == "tv" {
				date = info.FirstAirDate
			}
			out = append(out, tmdbTitleChoice{Kind: mediaType, Info: item, date: date})
		}
	}
	if merge {
		sort.SliceStable(out, func(i, j int) bool { return out[i].date > out[j].date })
		start := min((q.Page-1)*20, len(out))
		out = out[start:min(start+20, len(out))]
	}
	return out, nil
}

func chatChoiceCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	var req struct {
		Channel, Sender, Message, Revision string
		ChatEnabled                        bool
		Choices                            []json.RawMessage
	}
	input := &io.LimitedReader{R: in, N: 2 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || req.Channel == "" || len(req.Channel) > 256 || req.Sender == "" || len(req.Sender) > 1024 {
		return errors.New("invalid conversation")
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
		return errors.New("message database unavailable")
	}
	store, err := notificationconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if strings.HasPrefix(req.Channel, "TG/") {
		channel, err := store.TelegramReceiver(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cfg map[string]any
		token := ""
		if err == nil && json.Unmarshal([]byte(channel.Config), &cfg) == nil {
			token = text(cfg["token"])
		}
		expected := fmt.Sprintf("TG/%d/%x", channel.ID, sha256.Sum256([]byte(token)))
		if !validTelegramToken(token) || req.Channel != expected || !telegramSenderAllowed(cfg, req.Sender, req.Message) {
			return json.NewEncoder(out).Encode(notificationconfig.ChatChoiceReply{Action: "ignore"})
		}
	}
	var reply notificationconfig.ChatChoiceReply
	if op == "start" {
		reply, err = store.StartChatChoice(ctx, req.Channel, req.Sender, req.Message, req.ChatEnabled)
	} else {
		reply, err = store.PublishChatChoices(ctx, req.Channel, req.Sender, req.Revision, req.Choices)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(reply)
}
