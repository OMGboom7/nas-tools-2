package mediaserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/movielike"
)

func favoriteRefreshSource(ctx context.Context, r refreshReader, reference string) (string, error) {
	p, err := movielike.Read(ctx, r, reference)
	if err != nil || p.State != "completed" || len(p.Work) == 0 || len(p.Proof) == 0 {
		return "", ErrRefreshState
	}
	raw, err := json.Marshal([]any{p.Definition, p.Work, p.Proof, p.Updated})
	return string(raw), err
}

// An actual completed reclassification produces the old explicit refresh
// event. It is independent of intake enrichment and never fabricates history.
func EnqueueFavoriteRefresh(ctx context.Context, tx *sql.Tx, reference string) error {
	raw, err := refreshSetting(ctx, tx, RefreshControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeRefreshControl(raw)
	if err != nil {
		return err
	}
	if !c.Authorized {
		return nil
	}
	if err := refreshControlCurrent(ctx, tx, c); errors.Is(err, ErrRefreshState) {
		return nil
	} else if err != nil {
		return err
	}
	source, err := movielike.Read(ctx, tx, reference)
	if err != nil || source.State != "completed" {
		return ErrRefreshState
	}
	var work struct {
		Scope  struct{ Root struct{ Path string } }
		Target string
	}
	if json.Unmarshal(source.Work, &work) != nil {
		return ErrRefreshState
	}
	title := source.Event.Name
	if title == "" {
		title = path.Base(work.Target)
	}
	item := RefreshItem{Title: title, Type: "MOV", Category: path.Dir(work.Target), TargetRoot: work.Scope.Root.Path, TargetPath: path.Join(work.Scope.Root.Path, work.Target), Directory: true}
	at, e1 := time.Parse(time.RFC3339Nano, source.Updated)
	granted, e2 := time.Parse(time.RFC3339Nano, c.Created)
	if e1 != nil || e2 != nil {
		return ErrRefreshState
	}
	if at.Before(granted) {
		return nil
	}
	d := RefreshDefinition{Actor: c.Actor, RequestID: RefreshDigest([]string{"movie-like", reference, c.Epoch})[:32], Source: "movie_like", Reference: reference, Control: c, Items: []RefreshItem{item}}
	d, err = bindRefreshSource(ctx, tx, d)
	if err != nil {
		return err
	}
	_, err = createRefreshJob(ctx, tx, d, at)
	return err
}
