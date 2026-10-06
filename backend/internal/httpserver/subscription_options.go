package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
)

type subscriptionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type subscriptionOptions struct {
	RSSSites         []subscriptionOption `json:"rssSites"`
	SearchSites      []subscriptionOption `json:"searchSites"`
	FilterRules      []subscriptionOption `json:"filterRules"`
	DownloadSettings []subscriptionOption `json:"downloadSettings"`
	SavePaths        []string             `json:"savePaths"`
	Warnings         []string             `json:"warnings"`
}

func (service subscriptionService) serveOptions(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		name  string
		value map[string]any
		err   error
	}
	requests := []string{"rssSites", "searchSites", "filterRules", "downloadSettings", "savePaths"}
	results := make(chan result, len(requests))
	var wait sync.WaitGroup
	for _, name := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if name == "searchSites" {
				value, handled, err := service.nativeIndexerOptions(ctx)
				if !handled && err == nil {
					err = errors.New("native indexer options unavailable")
				}
				results <- result{name, value, err}
				return
			}
			if name == "filterRules" && service.filters != nil {
				value, err := nativeFilterOptions(ctx, service.filters)
				results <- result{name, value, err}
				return
			}
			if name == "rssSites" && service.sites != nil {
				values, err := (siteService{store: service.sites}).nativeSiteList(ctx, true)
				results <- result{name, map[string]any{"code": 0, "data": map[string]any{"sites": values}}, err}
				return
			}
			if name == "downloadSettings" && service.downloaders != nil {
				options, err := nativeDownloadSettingOptions(ctx, service.downloaders)
				values := make([]any, 0, len(options))
				for _, option := range options {
					values = append(values, map[string]any{"id": option.Value, "name": option.Label})
				}
				results <- result{name, map[string]any{"code": 0, "success": true, "data": map[string]any{"data": values}}, err}
				return
			}
			if name == "savePaths" && service.downloaders != nil && service.systemConfig != nil {
				paths, err := nativeDownloadSavePaths(ctx, service.downloaders, service.systemConfig, "")
				values := make([]any, 0, len(paths))
				for _, path := range paths {
					values = append(values, path)
				}
				results <- result{name, map[string]any{"code": 0, "success": true, "data": map[string]any{"paths": values}}, err}
				return
			}
			results <- result{name: name, err: errors.New("native option store unavailable")}
		}()
	}
	wait.Wait()
	close(results)
	data := subscriptionOptions{RSSSites: []subscriptionOption{}, SearchSites: []subscriptionOption{}, FilterRules: []subscriptionOption{}, DownloadSettings: []subscriptionOption{}, SavePaths: []string{}, Warnings: []string{}}
	for result := range results {
		if result.err != nil || number(result.value["code"]) != 0 {
			data.Warnings = append(data.Warnings, result.name+" unavailable")
			continue
		}
		payload := legacyPayload(result.value)
		switch result.name {
		case "rssSites":
			data.RSSSites = normalizeNamedOptions(payload["sites"])
		case "searchSites":
			data.SearchSites = normalizeNamedOptions(payload["indexers"])
		case "filterRules":
			data.FilterRules = normalizeNamedOptions(payload["ruleGroups"])
		case "downloadSettings":
			data.DownloadSettings = normalizeNamedOptions(payload["data"])
		case "savePaths":
			data.SavePaths = stringsOf(payload["paths"])
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func normalizeNamedOptions(value any) []subscriptionOption {
	options := make([]subscriptionOption, 0)
	values := entries(value)
	if list, ok := value.([]any); ok {
		values = make([]valueEntry, 0, len(list))
		for index, item := range list {
			values = append(values, valueEntry{key: text(index), value: item})
		}
	}
	for _, entry := range values {
		label, optionValue := "", entry.key
		switch item := entry.value.(type) {
		case string:
			label = item
		case map[string]any:
			label = text(item["name"])
			if label == "" {
				label = text(item["title"])
			}
			if id := text(item["id"]); id != "" {
				optionValue = id
			}
		}
		if label == "" {
			label = optionValue
		}
		if optionValue != "" {
			options = append(options, subscriptionOption{Value: optionValue, Label: label})
		}
	}
	sort.SliceStable(options, func(i, j int) bool { return options[i].Label < options[j].Label })
	return options
}
