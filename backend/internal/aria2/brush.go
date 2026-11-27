package aria2

import (
	"context"
	"strconv"
)

// GlobalStat reports all waiting jobs, including those outside the UI page.
func (c *Client) BrushStats(ctx context.Context) (upload, download, unfinished int64, err error) {
	var stats map[string]string
	if err = c.call(ctx, "nastool-brush", "aria2.getGlobalStat", nil, &stats); err != nil {
		return
	}
	var active, waiting int64
	for key, dest := range map[string]*int64{"uploadSpeed": &upload, "downloadSpeed": &download, "numActive": &active, "numWaiting": &waiting} {
		n, e := strconv.ParseUint(stats[key], 10, 62)
		if e != nil {
			return 0, 0, 0, ErrResponse
		}
		*dest = int64(n)
	}
	return upload, download, active + waiting, nil
}
