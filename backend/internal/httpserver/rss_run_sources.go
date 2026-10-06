package httpserver

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

func (api *rssRunAPI) fetchRunArticles(ctx context.Context, task rsstaskconfig.Task, configuration map[string]any) ([]rssPreviewArticle, *recognitionFailure) {
	fail := func(status int, message string) ([]rssPreviewArticle, *recognitionFailure) {
		return nil, &recognitionFailure{status, message}
	}
	addresses, parsers := rssStringList(task.Address), rssValueList(task.Parser)
	if len(addresses) == 0 || len(addresses) > 100 || len(addresses) != len(parsers) {
		return fail(422, "invalid RSS sources")
	}
	articles := []rssPreviewArticle{}
	for i, address := range addresses {
		parserID, err := strconv.ParseInt(text(parsers[i]), 10, 64)
		if err != nil || parserID <= 0 {
			return fail(422, "invalid RSS parser selection")
		}
		parser, err := api.preview.parsers.Get(ctx, parserID)
		if err != nil {
			return fail(422, "RSS parser is unavailable")
		}
		var format rssFormat
		if json.Unmarshal([]byte(parser.Format), &format) != nil || format.List == "" || len(format.Item) == 0 {
			return fail(422, "invalid RSS parser format")
		}
		body, err := api.preview.fetch(ctx, address, parser.Params, task.Note, configuration)
		if err != nil {
			return fail(502, "RSS feed could not be retrieved")
		}
		items, err := parseRSSPreview(body, parser.Type, format, i+1)
		if err != nil {
			return fail(502, "RSS feed could not be parsed")
		}
		articles = append(articles, items...)
		if len(articles) > 1000 {
			return fail(422, "RSS resource limit exceeded")
		}
	}
	return articles, nil
}
