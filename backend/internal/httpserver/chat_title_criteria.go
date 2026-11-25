package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Catalog names are exact space-separated tokens in the original chat syntax.
// A station can belong to both lists. Removing it from the title must not hide
// it from the second list or remove a substring from the actual film name.
func chatTitleCriteria(in io.Reader, out io.Writer) error {
	var request struct {
		Title         string
		RSS, Indexers []string
		Settings      []struct {
			Name string
			ID   json.RawMessage
		}
	}
	input := &io.LimitedReader{R: in, N: 2 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 {
		return errors.New("invalid chat title filters")
	}
	words := strings.Fields(request.Title)
	available := make(map[string]bool, len(words))
	for _, word := range words {
		available[word] = true
	}
	remove := map[string]bool{}
	match := func(catalog []string) []string {
		selected := []string{}
		seen := map[string]bool{}
		for _, name := range catalog {
			if available[name] && !seen[name] {
				seen[name], remove[name] = true, true
				selected = append(selected, name)
			}
		}
		return selected
	}
	rss, indexers := match(request.RSS), match(request.Indexers)
	var setting json.RawMessage
	for _, entry := range request.Settings {
		if !available[entry.Name] {
			continue
		}
		remove[entry.Name] = true
		if setting == nil {
			setting = entry.ID
		}
	}
	title := []string{}
	for _, word := range words {
		if !remove[word] {
			title = append(title, word)
		}
	}
	return json.NewEncoder(out).Encode(map[string]any{"title": strings.Join(title, " "), "rss": rss, "indexers": indexers, "setting": setting})
}
