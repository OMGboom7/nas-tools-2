package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloaderhelper"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func sameHelperTask(a, b downloaderhelper.Task) bool {
	return a.Hash == b.Hash && a.Added == b.Added && a.Directory == b.Directory && a.Name == b.Name && a.Site == b.Site
}
func helperObserved(item helperItem, rows []downloaderhelper.Task) bool {
	if item.Action == "remove" {
		return len(rows) == 0
	}
	if len(rows) != 1 || !sameHelperTask(rows[0], item.Task) {
		return false
	}
	if item.Action == "seed" {
		return rows[0].Seeding
	}
	expected := downloaderhelper.LabelsAfter(item.Task.Labels, item.Want, item.Drop)
	return !slices.ContainsFunc(expected, func(label string) bool { return !slices.Contains(rows[0].Labels, label) }) && (item.Drop == "" || !slices.Contains(rows[0].Labels, item.Drop))
}

func (r *helperRun) apply(ctx context.Context, item helperItem, index int) (string, error) {
	var target helperTarget
	for _, v := range r.s.targets {
		if v.Item.ID == item.Downloader {
			target = v
			break
		}
	}
	d, t := target.Item, item.Task
	if d.ID == 0 {
		return "", downloaderhelper.ErrSettings
	}
	g, err := r.api.transfer.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(d.ID, t.Hash))
	if err != nil {
		return "", err
	}
	defer g.Close()
	c, err := r.client(d)
	if err != nil {
		return "failed", nil
	}
	id := bus.Hash([]any{r.id, index})
	object := bus.Hash([]any{c.Address(), t.Hash, t.Added, t.Directory, t.Name})
	store := r.api.transfer.system
	var existing, other string
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT state FROM GO_DOWNLOADER_HELPER_ACTION WHERE id=?", id).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT id FROM GO_DOWNLOADER_HELPER_ACTION WHERE object=? AND action=? AND state='pending'", object, item.Action).Scan(&other)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if existing != "" && existing != "pending" {
		return existing, nil
	}
	rows, err := c.HelperTasks(ctx, t.Hash)
	if err != nil {
		if other != "" {
			return "pending", nil
		}
		return "failed", nil
	}
	finish := func(key, state string) error {
		return store.Transaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE GO_DOWNLOADER_HELPER_ACTION SET state=? WHERE id=? AND state='pending'", state, key)
			return err
		})
	}
	if other != "" {
		if other == id && helperObserved(item, rows) {
			return item.Action, finish(id, item.Action)
		}
		return "pending", nil
	}
	if len(rows) != 1 || !sameHelperTask(rows[0], t) || !slices.Equal(t.Labels, rows[0].Labels) {
		return "skipped", nil
	}
	valid := func(ctx context.Context) error {
		if err := r.current(ctx); err != nil {
			return err
		}
		fresh, err := c.HelperTasks(ctx, t.Hash)
		if err != nil || len(fresh) != 1 || !sameHelperTask(fresh[0], t) || downloaderhelper.Excluded(fresh[0].Labels, r.s.exclude) {
			return downloaderhelper.ErrSettings
		}
		live := fresh[0]
		// qB applies an addition and removal separately. Only our own addition
		// may explain changed labels between those two requests.
		if !slices.Equal(live.Labels, t.Labels) && !(item.Action == "tag" && d.Type == "qbittorrent" && slices.Equal(live.Labels, downloaderhelper.LabelsAfter(t.Labels, item.Want, ""))) {
			return downloaderhelper.ErrSettings
		}
		switch item.Action {
		case "seed":
			if !live.CanSeed {
				return downloaderhelper.ErrSettings
			}
		case "remove":
			if item.Payload == "" || item.Payload != r.removalProof(ctx, d, c, live) {
				return downloaderhelper.ErrSettings
			}
			if r.trigger.Kind == "sourcefile.deleted" {
				if !helperSourceGone(ctx, c, d, live, r.trigger) {
					return downloaderhelper.ErrSettings
				}
			} else if !live.Missing {
				return downloaderhelper.ErrSettings
			}
		}
		return r.workflowReady(ctx, c, t)
	}
	if err := valid(ctx); err != nil {
		return "skipped", nil
	}
	sent := false
	guarded := markTransport(func(req *http.Request) (*http.Response, error) {
		if err := valid(req.Context()); err != nil {
			return nil, err
		}
		mutation := helperMutation(req)
		tr := r.api.downloads.client.Transport
		if tr == nil {
			tr = http.DefaultTransport
		}
		res, err := tr.RoundTrip(req)
		if mutation && (err != nil || res == nil || res.StatusCode != http.StatusConflict) {
			sent = true
		}
		return res, err
	})
	ds := *r.api.downloads
	ds.client = &http.Client{Transport: guarded}
	// Claim the exact object before sending. A lost reply can be reconciled
	// by observing its effect, never by retransmitting the mutation.
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		b, err := json.Marshal(item)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO GO_DOWNLOADER_HELPER_ACTION VALUES(?,?,?,'pending',?,?)", id, object, item.Action, string(b), d.ID)
		return err
	})
	if err != nil {
		return "", err
	}
	if item.Action == "tag" {
		org := *r.api.transfer.organizer
		org.service.client = ds.client
		cc, e := org.clientFromDownloader(d)
		if e != nil {
			return "failed", finish(id, "failed")
		}
		err = cc.transfer.(helperClient).SetHelperLabels(ctx, t, item.Want, item.Drop)
	} else {
		_, err = ds.controlConfigured(ctx, d, t.Hash, item.Action)
	}
	if err != nil && !sent {
		return "failed", finish(id, "failed")
	}
	// A successful RPC may precede the daemon's state change. Poll only for
	// evidence, with a bounded wait and without reissuing the command.
	for attempt := 0; ; attempt++ {
		rows, readErr := c.HelperTasks(ctx, t.Hash)
		if readErr == nil && helperObserved(item, rows) {
			return item.Action, finish(id, item.Action)
		}
		if err != nil || readErr != nil || attempt >= 4 {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// Acknowledged start/removal can be asynchronous. Keep the receipt until
	// readback confirms it; losing an acknowledgement has the same outcome.
	return "pending", nil
}

// Native reseed/transfer verification owns its paused target and labels.
func (r *helperRun) workflowReady(ctx context.Context, c helperClient, t downloaderhelper.Task) error {
	return r.api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		receiver := torrenttransfer.Hash(c.Address())
		has := func(table string) (bool, error) {
			var exists bool
			err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name=?)", table).Scan(&exists)
			return exists, err
		}
		check := func(query string, args ...any) error {
			var held bool
			if err := tx.QueryRowContext(ctx, query, args...).Scan(&held); err != nil {
				return err
			}
			if held {
				return downloaderhelper.ErrSettings
			}
			return nil
		}
		if exists, err := has("GO_IYUU_SEEDS"); err != nil {
			return err
		} else if exists {
			if err := check("SELECT EXISTS(SELECT 1 FROM GO_IYUU_SEEDS WHERE receiver=? AND hash=? AND state NOT IN ('seeding','unsent','duplicate'))", receiver, t.Hash); err != nil {
				return err
			}
		}
		exists, err := has("GO_TORRENT_TRANSFER_PLANS")
		if err != nil || !exists {
			return err
		}
		// Older plans can prove completion from a confirmed source removal.
		// A start acknowledgement alone still allows pending source removal.
		released := "EXISTS(SELECT 1 FROM GO_TORRENT_TRANSFER_ABSENCE e WHERE e.receipt=p.receipt)"
		if exists, err := has("GO_TORRENT_TRANSFER_COMPLETED"); err != nil {
			return err
		} else if exists {
			released += " OR EXISTS(SELECT 1 FROM GO_TORRENT_TRANSFER_COMPLETED e WHERE e.receipt=p.receipt)"
		}
		return check(`SELECT EXISTS(SELECT 1 FROM GO_TORRENT_TRANSFER_ACTIONS a
			JOIN GO_TORRENT_TRANSFER_PLANS p ON p.receipt=a.receipt
			LEFT JOIN GO_TORRENT_TRANSFER_TARGETS t ON t.receipt=p.receipt
			LEFT JOIN GO_TORRENT_TRANSFER_ADD_REPLIES r ON r.receipt=p.receipt
			WHERE a.receiver=? AND a.wire_id=? AND a.action='add' AND COALESCE(r.duplicate,0)=0
			AND (t.task IS NULL OR json_extract(t.task,'$.addedAt')=?) AND NOT (`+released+`))`, receiver, t.Hash, t.Added)
	})
}

// Record only actual downloader mutations. Transmission can reject an initial
// request with HTTP 409 to negotiate the session before sending it for real.
func helperMutation(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	if strings.Contains(req.URL.Path, "/api/v2/torrents/") {
		return true
	}
	if !strings.HasSuffix(req.URL.Path, "/transmission/rpc") || req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	var p struct{ Method string }
	if json.NewDecoder(body).Decode(&p) != nil {
		return false
	}
	return slices.Contains([]string{"torrent_set", "torrent-set", "torrent_start", "torrent-start", "torrent_remove", "torrent-remove"}, p.Method)
}

// A later scan may observe an acknowledged effect after the originating batch
// finished. Clear its outstanding claim before planning the next operation.
func (r *helperRun) reconcile(ctx context.Context, c helperClient, target helperTarget, tasks []downloaderhelper.Task) error {
	byHash := map[string][]downloaderhelper.Task{}
	for _, t := range tasks {
		byHash[t.Hash] = []downloaderhelper.Task{t}
	}
	return r.api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id,object,desired FROM GO_DOWNLOADER_HELPER_ACTION WHERE state='pending' AND downloader=?", target.Item.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		done := map[string]string{}
		for rows.Next() {
			var id, object, raw string
			var item helperItem
			if err := rows.Scan(&id, &object, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				return err
			}
			t := item.Task
			if object == bus.Hash([]any{c.Address(), t.Hash, t.Added, t.Directory, t.Name}) && helperObserved(item, byHash[t.Hash]) {
				done[id] = item.Action
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for id, kind := range done {
			if _, err := tx.ExecContext(ctx, "UPDATE GO_DOWNLOADER_HELPER_ACTION SET state=? WHERE id=? AND state='pending'", kind, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Removing a qB/TR task with its data affects every torrent that points at the
// same file. Freeze the file layout and check neighboring tasks on the daemon
// before sending either protocol's delete-data operation.
func (r *helperRun) removalProof(ctx context.Context, d downloaderconfig.Downloader, c helperClient, t downloaderhelper.Task) string {
	get := func(row downloaderhelper.Task) (torrenttransfer.Task, string, bool) {
		s, err := c.TransferTask(ctx, row.Hash)
		dir, e := torrenttransfer.NormalizeRemote(s.Directory)
		return s, dir, err == nil && e == nil && s.AddedAt == row.Added && s.Name == row.Name && s.Directory == row.Directory && len(s.Files) > 0
	}
	src, dir, ok := get(t)
	if !ok {
		return ""
	}
	files := map[string]bool{}
	for _, f := range src.Files {
		files[strings.ToLower(strings.TrimRight(dir, "/")+"/"+f.Path)] = true
	}
	tasks, err := c.HelperTasks(ctx, "")
	if err != nil {
		return ""
	}
	for _, other := range tasks {
		if other.Hash == t.Hash {
			continue
		}
		base, err := torrenttransfer.NormalizeRemote(other.Directory)
		if err != nil {
			return ""
		}
		a, b := strings.ToLower(base), strings.ToLower(dir)
		if a != b && !strings.HasPrefix(a, strings.TrimRight(b, "/")+"/") && !strings.HasPrefix(b, strings.TrimRight(a, "/")+"/") {
			continue
		}
		row, otherDir, ok := get(other)
		if !ok {
			return ""
		}
		for _, f := range row.Files {
			if files[strings.ToLower(strings.TrimRight(otherDir, "/")+"/"+f.Path)] {
				return ""
			}
		}
	}
	if !r.separateStorage(ctx, d, src) {
		return ""
	}
	return bus.Hash([]any{src.WireID, src.AddedAt, dir, src.Files})
}

// Different downloaders can mount the same NAS directory under different
// names. Inspect overlapping peers only if included in the helper settings.
func (r *helperRun) separateStorage(ctx context.Context, d downloaderconfig.Downloader, src torrenttransfer.Task) bool {
	peers, err := r.api.transfer.downloaders.List(ctx)
	if err != nil {
		return false
	}
	local, err := downloadcompletion.Resolve(d.DownloadDir, src.Directory, d.MatchPath == 1)
	files := map[string]bool{}
	if err == nil {
		for _, f := range src.Files {
			files[strings.ToLower(path.Join(local.Directory, f.Path))] = true
		}
	}
	for _, peer := range peers {
		if peer.ID == d.ID {
			continue
		}
		var mappings []downloadcompletion.Mapping
		if len(files) == 0 || json.Unmarshal([]byte(peer.DownloadDir), &mappings) != nil || len(mappings) == 0 {
			return false
		}
		overlap := false
		for _, m := range mappings {
			root := strings.TrimSuffix(m.ContainerPath, "/")
			if root == "" {
				root = strings.TrimSuffix(m.SavePath, "/")
			}
			if !downloadcompletion.Absolute(root) {
				return false
			}
			root = strings.ToLower(root)
			for file := range files {
				if file == root || strings.HasPrefix(file, root+"/") {
					overlap = true
					break
				}
			}
		}
		if !overlap {
			continue
		}
		if !slices.ContainsFunc(r.s.targets, func(t helperTarget) bool { return t.Item == peer }) {
			return false
		}
		client, err := r.client(peer)
		if err != nil {
			return false
		}
		tasks, err := client.HelperTasks(ctx, "")
		if err != nil {
			return false
		}
		for _, task := range tasks {
			loc, err := downloadcompletion.Resolve(peer.DownloadDir, task.Directory, peer.MatchPath == 1)
			if err != nil {
				return false
			}
			candidate := strings.ToLower(loc.Directory) + "/"
			found := false
			for file := range files {
				if strings.HasPrefix(file, candidate) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			full, err := client.TransferTask(ctx, task.Hash)
			if err != nil || full.AddedAt != task.Added || full.Directory != task.Directory || len(full.Files) == 0 {
				return false
			}
			for _, f := range full.Files {
				if files[strings.ToLower(path.Join(loc.Directory, f.Path))] {
					return false
				}
			}
		}
	}
	return true
}
