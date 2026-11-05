package backuprestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Entry struct {
	Target   string `json:"target"`
	Before   File   `json:"before"`
	Incoming File   `json:"incoming"`
}

type Transaction struct {
	ID      string  `json:"id"`
	Phase   string  `json:"phase"`
	Config  string  `json:"config"`
	Entries []Entry `json:"entries"`
}

func (w Workspace) targets() []string {
	return []string{"user.db-wal", "user.db-shm", "user.db-journal", "user.db.pre-go.bak", w.config + ".bak", "user.db", "default-category.yaml", w.config}
}

func (w Workspace) journal() string { return filepath.Join(w.private(), "transaction.json") }

// HoldStartup checks the journal before normal startup can open SQLite.
func HoldStartup(configPath string) (func(), error) {
	if configPath == "" {
		return func() {}, nil
	}
	w, err := New(configPath)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		_, err := os.Lstat(w.private())
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
		return nil, ErrHold
	}
	lock, err := w.lock(false)
	if err != nil {
		return nil, err
	}
	_, err = w.transaction()
	if !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrHold
	}
	// A resume may have removed the visible journal just before its final
	// fsync failed. Make that removal durable before opening the live DB.
	if err = privateDir(w.private(), false); err == nil {
		err = syncDir(w.private())
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func (w Workspace) transaction() (Transaction, error) {
	var tx Transaction
	if err := privateDir(w.private(), false); err != nil {
		return tx, err
	}
	if err := readJSON(w.journal(), &tx); err != nil {
		return tx, err
	}
	if !idPattern.MatchString(tx.ID) || tx.Config != w.config {
		return Transaction{}, ErrState
	}
	switch tx.Phase {
	case "applying", "applied", "rolling-back", "rolled-back":
	default:
		return Transaction{}, ErrState
	}
	targets := w.targets()
	if len(tx.Entries) != len(targets) {
		return Transaction{}, ErrState
	}
	for i, entry := range tx.Entries {
		if entry.Target != targets[i] || entry.Before.Name != "" && entry.Before.Name != entry.Target || entry.Incoming.Name != w.archiveName(entry.Target) {
			return Transaction{}, ErrState
		}
		if !validFingerprint(entry.Before) || !validFingerprint(entry.Incoming) {
			return Transaction{}, ErrState
		}
	}
	return tx, nil
}

func validFingerprint(f File) bool {
	if f.Name == "" {
		return f == (File{})
	}
	return f.Size >= 0 && len(f.SHA256) == 64 && strings.IndexFunc(f.SHA256, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

func (w Workspace) archiveName(target string) string {
	switch target {
	case w.config:
		return "config.yaml"
	case "user.db", "default-category.yaml":
		return target
	default:
		return ""
	}
}

func (w Workspace) Status() (Transaction, error) {
	f, err := w.lock(false)
	if err != nil {
		return Transaction{}, err
	}
	defer f.Close()
	return w.transaction()
}

func digest(ctx context.Context, path, name string) (File, error) {
	f, err := inspectFile(ctx, path, name)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	return f, err
}

func (w Workspace) newTransaction(ctx context.Context, id string) (Transaction, error) {
	stage, err := w.Receipt(id)
	if err != nil {
		return Transaction{}, err
	}
	result := filepath.Join(w.candidate(id), "result.json")
	if _, err := os.Lstat(result); !errors.Is(err, os.ErrNotExist) {
		return Transaction{}, ErrState
	}
	path := w.candidate(id)
	for _, dir := range []string{"incoming", "previous"} {
		if err := privateDir(filepath.Join(path, dir), false); err != nil {
			return Transaction{}, err
		}
	}
	incoming := map[string]File{}
	for _, file := range stage.Files {
		switch file.Name {
		case "config.yaml", "default-category.yaml", "user.db":
		default:
			return Transaction{}, ErrState
		}
		if !validFingerprint(file) || incoming[file.Name].Name != "" {
			return Transaction{}, ErrState
		}
		before, err := digest(ctx, filepath.Join(path, "incoming", file.Name), file.Name)
		if err != nil || before != file {
			return Transaction{}, ErrState
		}
		incoming[file.Name] = file
	}
	previous, err := os.ReadDir(filepath.Join(path, "previous"))
	if err != nil || len(previous) != 0 {
		return Transaction{}, ErrState
	}
	tx := Transaction{ID: id, Phase: "applying", Config: w.config}
	for _, name := range w.targets() {
		before, err := digest(ctx, filepath.Join(w.root, name), name)
		if err != nil {
			return Transaction{}, err
		}
		tx.Entries = append(tx.Entries, Entry{Target: name, Before: before, Incoming: incoming[w.archiveName(name)]})
	}
	return tx, nil
}

func checkedMove(from, to string) error {
	if err := moveExclusive(from, to); err != nil {
		return err
	}
	return errors.Join(syncDir(filepath.Dir(from)), syncDir(filepath.Dir(to)))
}

type paths struct{ live, previous, incoming string }

func (w Workspace) paths(id string, e Entry) paths {
	root := w.candidate(id)
	return paths{filepath.Join(w.root, e.Target), filepath.Join(root, "previous", e.Target), filepath.Join(root, "incoming", e.Incoming.Name)}
}

func (p paths) current(ctx context.Context, e Entry) (live, prior, next File, err error) {
	live, err = digest(ctx, p.live, e.Target)
	if err != nil {
		return
	}
	prior, err = digest(ctx, p.previous, e.Target)
	if err != nil || e.Incoming.Name == "" {
		return
	}
	next, err = digest(ctx, p.incoming, e.Incoming.Name)
	return
}

func matches(a, b File) bool {
	return a.Size == b.Size && a.SHA256 == b.SHA256 && (a.Name == "") == (b.Name == "")
}

func applyEntry(ctx context.Context, p paths, e Entry) error {
	live, prior, next, err := p.current(ctx, e)
	if err != nil {
		return err
	}
	if e.Before.Name != "" && prior.Name == "" {
		if !matches(live, e.Before) || next != e.Incoming {
			return ErrState
		}
		if err := checkedMove(p.live, p.previous); err != nil {
			return err
		}
		live = File{}
	} else if prior != e.Before {
		return ErrState
	}
	if next == e.Incoming && next.Name != "" {
		if live.Name != "" {
			return ErrState
		}
		return checkedMove(p.incoming, p.live)
	}
	if next.Name != "" || !matches(live, e.Incoming) {
		return ErrState
	}
	return nil
}

func rollbackEntry(ctx context.Context, p paths, e Entry) error {
	live, prior, next, err := p.current(ctx, e)
	if err != nil {
		return err
	}
	if e.Incoming.Name != "" && next.Name == "" {
		if !matches(live, e.Incoming) || prior != e.Before {
			return ErrState
		}
		if err := checkedMove(p.live, p.incoming); err != nil {
			return err
		}
		live, next = File{}, e.Incoming
	}
	if next != e.Incoming {
		return ErrState
	}
	if prior.Name != "" {
		if prior != e.Before || live.Name != "" {
			return ErrState
		}
		return checkedMove(p.previous, p.live)
	}
	if live != e.Before {
		return ErrState
	}
	return nil
}

func (w Workspace) Apply(ctx context.Context, id string, offline bool) (Transaction, error) {
	if !offline || !idPattern.MatchString(id) {
		return Transaction{}, ErrState
	}
	lock, err := w.lock(true)
	if err != nil {
		return Transaction{}, err
	}
	defer lock.Close()
	tx, err := w.transaction()
	if errors.Is(err, os.ErrNotExist) {
		tx, err = w.newTransaction(ctx, id)
		if err == nil {
			err = writeJSON(w.journal(), tx)
		}
	}
	if err != nil {
		return tx, err
	}
	if tx.ID != id || tx.Phase != "applying" && tx.Phase != "applied" {
		return tx, ErrState
	}
	if err := w.validateStageDirs(id); err != nil {
		return tx, err
	}
	if err := syncDir(w.private()); err != nil {
		return tx, err
	}
	for _, entry := range tx.Entries {
		if err := applyEntry(ctx, w.paths(id, entry), entry); err != nil {
			return tx, fmt.Errorf("恢复 %s: %w", entry.Target, err)
		}
	}
	if err := w.syncStage(id); err != nil {
		return tx, err
	}
	tx.Phase = "applied"
	return tx, writeJSON(w.journal(), tx)
}

func (w Workspace) validateStageDirs(id string) error {
	for _, path := range []string{w.candidate(id), filepath.Join(w.candidate(id), "incoming"), filepath.Join(w.candidate(id), "previous")} {
		if err := privateDir(path, false); err != nil {
			return err
		}
	}
	return nil
}

func (w Workspace) syncStage(id string) error {
	path := w.candidate(id)
	return errors.Join(syncDir(w.root), syncDir(filepath.Join(path, "incoming")), syncDir(filepath.Join(path, "previous")))
}

func (w Workspace) Rollback(ctx context.Context, id string, offline bool) (Transaction, error) {
	if !offline || !idPattern.MatchString(id) {
		return Transaction{}, ErrState
	}
	lock, err := w.lock(true)
	if err != nil {
		return Transaction{}, err
	}
	defer lock.Close()
	tx, err := w.transaction()
	if err != nil {
		return tx, err
	}
	if tx.ID != id {
		return tx, ErrState
	}
	if err := w.validateStageDirs(id); err != nil {
		return tx, err
	}
	if tx.Phase != "rolling-back" && tx.Phase != "rolled-back" {
		tx.Phase = "rolling-back"
		if err := writeJSON(w.journal(), tx); err != nil {
			return tx, err
		}
	}
	if err := syncDir(w.private()); err != nil {
		return tx, err
	}
	for i := len(tx.Entries) - 1; i >= 0; i-- {
		entry := tx.Entries[i]
		if err := rollbackEntry(ctx, w.paths(id, entry), entry); err != nil {
			return tx, fmt.Errorf("回滚 %s: %w", entry.Target, err)
		}
	}
	if err := w.syncStage(id); err != nil {
		return tx, err
	}
	tx.Phase = "rolled-back"
	return tx, writeJSON(w.journal(), tx)
}

func (w Workspace) Resume(ctx context.Context, id string, confirm bool, preflight func(string) error) (Transaction, error) {
	if !confirm || !idPattern.MatchString(id) {
		return Transaction{}, ErrState
	}
	lock, err := w.lock(true)
	if err != nil {
		return Transaction{}, err
	}
	defer lock.Close()
	tx, err := w.transaction()
	if err != nil {
		return tx, err
	}
	if tx.ID != id || tx.Phase != "applied" && tx.Phase != "rolled-back" {
		return tx, ErrHold
	}
	if err := w.validateStageDirs(id); err != nil {
		return tx, err
	}
	for _, entry := range tx.Entries {
		live, prior, next, err := w.paths(id, entry).current(ctx, entry)
		if err != nil {
			return tx, err
		}
		if tx.Phase == "applied" && (!matches(live, entry.Incoming) || prior != entry.Before || next.Name != "") ||
			tx.Phase == "rolled-back" && (live != entry.Before || prior.Name != "" || next != entry.Incoming) {
			return tx, ErrState
		}
	}
	if tx.Phase == "applied" {
		if preflight == nil {
			return tx, ErrState
		}
		if err := w.preflight(ctx, tx, preflight); err != nil {
			return tx, err
		}
	}
	if err := ctx.Err(); err != nil {
		return tx, err
	}
	if err := writeJSON(filepath.Join(w.candidate(id), "result.json"), tx); err != nil {
		return tx, err
	}
	if err := os.Remove(w.journal()); err != nil {
		return tx, err
	}
	return tx, syncDir(w.private())
}

func (w Workspace) preflight(ctx context.Context, tx Transaction, check func(string) error) error {
	dir, err := os.MkdirTemp("", "nastool-restore-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, entry := range tx.Entries {
		if entry.Incoming.Name == "" {
			continue
		}
		if err := copyChecked(ctx, w.paths(tx.ID, entry).live, filepath.Join(dir, entry.Target), entry.Incoming); err != nil {
			return err
		}
	}
	return check(filepath.Join(dir, tx.Config))
}

func copyChecked(ctx context.Context, src, dest string, want File) error {
	input, err := openRegular(src)
	if err != nil {
		return err
	}
	defer input.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(cancelReader{ctx, input}, want.Size+1))
	if err = errors.Join(err, out.Close()); err != nil {
		return err
	}
	if n != want.Size || hex.EncodeToString(hash.Sum(nil)) != want.SHA256 {
		return ErrState
	}
	return nil
}
