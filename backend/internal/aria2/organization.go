package aria2

import (
	"context"
	"strings"
)

func (c *Client) RemoveOrganizationTask(ctx context.Context, gid string) error {
	if !ValidGID(gid) {
		return ErrConfiguration
	}
	var ack string
	if err := c.call(ctx, "nastool-organized-result", "aria2.removeDownloadResult", []any{gid}, &ack); err != nil {
		return err
	}
	if ack != "OK" {
		return ErrResponse
	}
	return nil
}
func (c *Client) MarkOrganized(ctx context.Context, gid string, _ []string) error {
	return c.RemoveOrganizationTask(ctx, gid)
}

// Absence is proven by successful exhaustive bounded queue reads, never by a
// failed tellStatus call. At the bound, uncertainty is retained for review.
func (c *Client) HasOrganizationTask(ctx context.Context, gid string) (bool, error) {
	if !ValidGID(gid) {
		return false, ErrConfiguration
	}
	for _, method := range []string{"aria2.tellActive", "aria2.tellWaiting", "aria2.tellStopped"} {
		var tasks []struct {
			GID string `json:"gid"`
		}
		args := []any{[]string{"gid"}}
		if method != "aria2.tellActive" {
			args = []any{0, 1000, []string{"gid"}}
		}
		if err := c.call(ctx, "nastool-organized-presence", method, args, &tasks); err != nil {
			return false, err
		}
		if tasks == nil || len(tasks) > 1000 {
			return false, ErrResponse
		}
		for _, t := range tasks {
			if !ValidGID(t.GID) {
				return false, ErrResponse
			}
			if strings.EqualFold(t.GID, gid) {
				return true, nil
			}
		}
		if len(tasks) == 1000 {
			return false, ErrResponse
		}
	}
	return false, nil
}
