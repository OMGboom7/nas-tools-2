package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

func (service downloadService) restoreRSSAcknowledgement(ctx context.Context, key string, taskID int64, receipt, actor string) (downloaderconfig.ManualSubmission, bool, error) {
	r, done, err := service.downloaders.RestoreAcknowledgedRSS(ctx, key, taskID, receipt, actor)
	if done && r.Definition.DownloaderType == "qbittorrent" {
		if item, e := service.downloaders.Get(ctx, r.Definition.DownloaderID); e == nil && bus.Hash(item) == r.Definition.ConfigDigest {
			service.removeClassicMarker(ctx, item, r.TaskID, r.ID)
		}
	}
	return r, done, err
}

// Execution checks the durable acknowledgement before media lookup or fetching
// the torrent. Read-only feed previews use the ordinary history query.
func classicRSSStatus(ctx context.Context, dbPath string, in io.Reader, out io.Writer) error {
	var q struct {
		ID       int64
		Articles []rsstaskconfig.Article
	}
	input := &io.LimitedReader{R: in, N: 8 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || q.ID <= 0 {
		return errManualConfiguration
	}
	tasks, err := rsstaskconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer tasks.Close()
	downloaders, err := downloaderconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer downloaders.Close()
	service := downloadService{client: http.DefaultClient, downloaders: downloaders}
	states := make([]string, 0, len(q.Articles))
	for _, a := range q.Articles {
		state, err := service.rssArticleState(ctx, tasks, q.ID, a)
		if err != nil {
			return err
		}
		states = append(states, state)
	}
	return json.NewEncoder(out).Encode(states)
}

func (service downloadService) rssArticleState(ctx context.Context, tasks *rsstaskconfig.Store, id int64, a rsstaskconfig.Article) (string, error) {
	pending, err := tasks.HasPendingDownload(ctx, a.Enclosure)
	if err != nil {
		return "", err
	}
	if pending {
		actor := "classic:" + bus.Hash([]string{"USERRSS", "userrss:" + strconv.FormatInt(id, 10)})
		_, restored, err := service.restoreRSSAcknowledgement(ctx, rssreceipt.Key(a.Enclosure), id, "", actor)
		if restored {
			return "processed", nil
		}
		if err != nil {
			return "pending", nil
		}
		pending, err = tasks.HasPendingDownload(ctx, a.Enclosure)
		if err != nil || pending {
			return "pending", err
		}
	}
	seen, err := tasks.IsProcessed(ctx, "D", a.Title, a.Year, a.Enclosure)
	if err != nil {
		return "", err
	}
	if seen {
		return "processed", nil
	}
	return "new", nil
}
