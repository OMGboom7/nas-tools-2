package aria2

import (
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

// Completion omits source URIs, cookies and error messages from its evidence.
// Protocol: https://aria2.github.io/manual/en/html/aria2c.html#aria2.tellStatus
func (client *Client) Completion(ctx context.Context, gid string) (downloadcompletion.Task, error) {
	if !ValidGID(gid) {
		return downloadcompletion.Task{}, ErrConfiguration
	}
	var raw struct {
		GID       string `json:"gid"`
		Status    string `json:"status"`
		Directory string `json:"dir"`
		Total     string `json:"totalLength"`
		Done      string `json:"completedLength"`
		Error     string `json:"errorCode"`
		Files     []struct {
			Path     string `json:"path"`
			Length   string `json:"length"`
			Done     string `json:"completedLength"`
			Selected string `json:"selected"`
		} `json:"files"`
		BitTorrent *struct {
			Info *struct {
				Name string `json:"name"`
			} `json:"info"`
		} `json:"bittorrent"`
		FollowedBy []string `json:"followedBy"`
	}
	if err := client.call(ctx, "nastool-completion", "aria2.tellStatus", []any{gid, []string{"gid", "status", "dir", "totalLength", "completedLength", "errorCode", "files", "bittorrent", "followedBy"}}, &raw); err != nil {
		return downloadcompletion.Task{}, err
	}
	raw.Directory = strings.TrimRight(raw.Directory, "/")
	total, e1 := strconv.ParseInt(raw.Total, 10, 64)
	done, e2 := strconv.ParseInt(raw.Done, 10, 64)
	if !strings.EqualFold(raw.GID, gid) || e1 != nil || e2 != nil || total < 0 || done < 0 || done > total || !downloadcompletion.Absolute(raw.Directory) || len(raw.Files) == 0 || len(raw.Files) > 1000 {
		return downloadcompletion.Task{}, ErrResponse
	}
	switch raw.Status {
	case "active", "waiting", "paused", "error", "complete", "removed":
	default:
		return downloadcompletion.Task{}, ErrResponse
	}
	task := downloadcompletion.Task{ID: raw.GID, Directory: raw.Directory, State: raw.Status, Complete: raw.Status == "complete" && done == total && (raw.Error == "" || raw.Error == "0") && len(raw.FollowedBy) == 0}
	if raw.BitTorrent != nil && raw.BitTorrent.Info != nil {
		task.Name = raw.BitTorrent.Info.Name
	}
	for _, f := range raw.Files {
		if !downloadcompletion.Absolute(f.Path) || !strings.HasPrefix(f.Path, raw.Directory+"/") || f.Selected != "true" && f.Selected != "false" {
			return downloadcompletion.Task{}, ErrResponse
		}
		size, e1 := strconv.ParseInt(f.Length, 10, 64)
		done, e2 := strconv.ParseInt(f.Done, 10, 64)
		if e1 != nil || e2 != nil || size < 0 || done < 0 || done > size {
			return downloadcompletion.Task{}, ErrResponse
		}
		relative := strings.TrimPrefix(f.Path, raw.Directory+"/")
		task.Files = append(task.Files, downloadcompletion.File{Path: relative, Size: size, Selected: f.Selected == "true", Complete: done == size})
		if task.Name == "" {
			task.Name = path.Base(f.Path)
		}
	}
	result, err := downloadcompletion.Normalize(task)
	if err != nil {
		return downloadcompletion.Task{}, ErrResponse
	}
	return result, nil
}
