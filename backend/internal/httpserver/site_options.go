package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

type siteOptions struct {
	FilterRules      []subscriptionOption `json:"filterRules"`
	DownloadSettings []subscriptionOption `json:"downloadSettings"`
	Warnings         []string             `json:"warnings"`
}

func (service siteService) serveOptions(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	type optionResult struct {
		name  string
		value map[string]any
		err   error
	}
	requests := []string{"filterRules", "downloadSettings"}
	results := make(chan optionResult, len(requests))
	var wait sync.WaitGroup
	for _, name := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if name == "filterRules" {
				if service.filters == nil {
					results <- optionResult{name: name, err: errors.New("native filter store unavailable")}
					return
				}
				value, err := nativeFilterOptions(ctx, service.filters)
				results <- optionResult{name: name, value: value, err: err}
				return
			}
			if name == "downloadSettings" {
				if service.downloaders == nil {
					results <- optionResult{name: name, err: errors.New("native downloader store unavailable")}
					return
				}
				options, err := nativeDownloadSettingOptions(ctx, service.downloaders)
				values := make([]any, 0, len(options))
				for _, option := range options {
					values = append(values, map[string]any{"id": option.Value, "name": option.Label})
				}
				results <- optionResult{name: name, value: map[string]any{"code": 0, "success": true, "data": map[string]any{"data": values}}, err: err}
				return
			}
		}()
	}
	wait.Wait()
	close(results)

	data := siteOptions{FilterRules: []subscriptionOption{}, DownloadSettings: []subscriptionOption{}, Warnings: []string{}}
	unauthorized := 0
	for result := range results {
		if result.err != nil || number(result.value["code"]) != 0 {
			if result.err == nil && number(result.value["code"]) == 403 {
				unauthorized++
			}
			data.Warnings = append(data.Warnings, result.name+" unavailable")
			continue
		}
		payload := legacyPayload(result.value)
		if result.name == "filterRules" {
			data.FilterRules = normalizeNamedOptions(payload["ruleGroups"])
		} else {
			data.DownloadSettings = normalizeNamedOptions(payload["data"])
		}
	}
	if unauthorized == len(requests) {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}
