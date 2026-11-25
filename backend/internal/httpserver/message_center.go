package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

func MessageCenterCommand(ctx context.Context, path, action string, in io.Reader, out io.Writer) error {
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	path = (config.Application{Path: path}).UserDatabasePath()
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return errors.New("message database unavailable")
	}
	store, err := notificationconfig.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	input := &io.LimitedReader{R: in, N: 8 << 20}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	switch action {
	case "append":
		var item struct{ Title, Content string }
		if decoder.Decode(&item) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
			return errors.New("invalid message")
		}
		return store.AddSystemMessage(ctx, item.Title, item.Content)
	case "list":
		var query struct {
			Limit int
			After int64
			Since string
		}
		if decoder.Decode(&query) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
			return errors.New("invalid message query")
		}
		rows, err := store.SystemMessages(ctx, query.Limit, query.After, query.Since)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(rows)
	}
	return errors.New("unknown message command")
}

func (service notificationService) center(w http.ResponseWriter, r *http.Request) {
	if !service.requireStore(w) {
		return
	}
	items, err := service.store.SystemMessages(r.Context(), 50, 0, "")
	if err != nil {
		writeAPIError(w, 503, 503, "系统消息暂时不可用")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": items})
}
