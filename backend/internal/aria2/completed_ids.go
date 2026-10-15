package aria2

import (
	"context"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

func (c *Client) CompletedIDs(ctx context.Context) ([]string, error) {
	var rows []struct {
		GID    string `json:"gid"`
		Status string `json:"status"`
	}
	if err := c.call(ctx, "nastool-completed-candidates", "aria2.tellStopped", []any{0, 1001, []string{"gid", "status"}}, &rows); err != nil {
		return nil, err
	}
	if rows == nil || len(rows) > 1000 {
		return nil, ErrResponse
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, r := range rows {
		if !ValidGID(r.GID) || seen[r.GID] {
			return nil, ErrResponse
		}
		seen[r.GID] = true
		switch r.Status {
		case "complete":
			ids = append(ids, r.GID)
		case "removed", "error":
		default:
			return nil, ErrResponse
		}
	}
	var err error
	ids, err = downloadcompletion.NormalizeIDs("aria2", ids)
	if err != nil {
		return nil, ErrResponse
	}
	return ids, nil
}
