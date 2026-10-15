package qbittorrent

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

// Completion deliberately does not use the downloading-only Tasks endpoint.
// File names are relative to save_path, not inferred from the display name.
// Protocol: https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-4.1)#get-torrent-contents
func (client *Client) Completion(ctx context.Context, hash string) (downloadcompletion.Task, error) {
	fail := func() (downloadcompletion.Task, error) { return downloadcompletion.Task{}, ErrResponse }
	if !ValidHash(hash) {
		return downloadcompletion.Task{}, ErrConfiguration
	}
	if err := client.login(ctx); err != nil {
		return downloadcompletion.Task{}, err
	}
	raw, err := client.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"hashes": {hash}}.Encode(), nil)
	if err != nil {
		return downloadcompletion.Task{}, err
	}
	var tasks []struct {
		Hash        string   `json:"hash"`
		Name        string   `json:"name"`
		SavePath    string   `json:"save_path"`
		State       string   `json:"state"`
		Progress    *float64 `json:"progress"`
		Left        *int64   `json:"amount_left"`
		CompletedOn *int64   `json:"completion_on"`
		Tags        *string  `json:"tags"`
	}
	if json.Unmarshal(raw, &tasks) != nil || len(tasks) != 1 {
		return fail()
	}
	t := tasks[0]
	if !strings.EqualFold(t.Hash, hash) || t.Progress == nil || t.Left == nil || t.CompletedOn == nil || *t.Left < 0 || *t.CompletedOn < 0 || !validCompletionProgress(*t.Progress) {
		return fail()
	}
	completeState := false
	switch t.State {
	case "uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP":
		completeState = true
	}
	task := downloadcompletion.Task{ID: t.Hash, Name: t.Name, Directory: t.SavePath, State: t.State, Complete: completeState && *t.Progress == 1 && *t.Left == 0 && *t.CompletedOn > 0, LabelsKnown: t.Tags != nil}
	if t.Tags != nil {
		for _, tag := range strings.Split(*t.Tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				task.Labels = append(task.Labels, tag)
			}
		}
	}
	raw, err = client.request(ctx, http.MethodGet, "torrents/files?"+url.Values{"hash": {hash}}.Encode(), nil)
	if err != nil {
		return downloadcompletion.Task{}, err
	}
	var files []struct {
		Name     string   `json:"name"`
		Size     *int64   `json:"size"`
		Progress *float64 `json:"progress"`
		Priority *int     `json:"priority"`
	}
	if json.Unmarshal(raw, &files) != nil || len(files) == 0 || len(files) > 1000 {
		return fail()
	}
	for _, f := range files {
		if f.Size == nil || f.Progress == nil || f.Priority == nil || !validCompletionProgress(*f.Progress) {
			return fail()
		}
		switch *f.Priority {
		case 0, 1, 6, 7:
		default:
			return fail()
		}
		task.Files = append(task.Files, downloadcompletion.File{Path: f.Name, Size: *f.Size, Selected: *f.Priority > 0, Complete: *f.Progress == 1})
	}
	result, err := downloadcompletion.Normalize(task)
	if err != nil {
		return fail()
	}
	return result, nil
}
func validCompletionProgress(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
