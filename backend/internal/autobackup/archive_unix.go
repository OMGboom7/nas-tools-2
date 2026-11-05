//go:build darwin || linux

package autobackup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

var archiveName = regexp.MustCompile(`^bk_[0-9]{14}_[0-9a-f]{64}\.zip$`)

func dirIdentity(file *os.File) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return "", ErrScope
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// Review never creates directories. A missing leaf is bound to the existing parent.
func Review(path string) (Target, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return Target{}, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	d, err := openDirectory(path)
	if errors.Is(err, os.ErrNotExist) {
		d, err = openDirectory(parent)
		if err != nil {
			return Target{}, err
		}
		defer d.Close()
		id, err := dirIdentity(d)
		return Target{Path: path, Identity: "new:" + id}, err
	}
	if err != nil {
		return Target{}, err
	}
	defer d.Close()
	id, err := dirIdentity(d)
	return Target{Path: path, Identity: id}, err
}

func Prepare(target Target) (Target, error) {
	current, err := Review(target.Path)
	if err != nil || current != target {
		return Target{}, ErrScope
	}
	if !strings.HasPrefix(target.Identity, "new:") {
		return target, nil
	}
	parent, err := openDirectory(filepath.Dir(target.Path))
	if err != nil {
		return Target{}, err
	}
	defer parent.Close()
	id, err := dirIdentity(parent)
	if err != nil || "new:"+id != target.Identity {
		return Target{}, ErrScope
	}
	leaf := filepath.Base(target.Path)
	if err := unix.Mkdirat(int(parent.Fd()), leaf, 0700); err != nil {
		return Target{}, err
	}
	fd, err := unix.Openat(int(parent.Fd()), leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Target{}, err
	}
	created := os.NewFile(uintptr(fd), leaf)
	defer created.Close()
	identity, err := dirIdentity(created)
	if err != nil {
		return Target{}, err
	}
	prepared := Target{Path: target.Path, Identity: identity}
	return prepared, prepared.Verify()
}

func (t Target) open() (*os.File, error) {
	d, err := openDirectory(t.Path)
	if err != nil {
		return nil, err
	}
	id, err := dirIdentity(d)
	if err != nil || id != t.Identity {
		d.Close()
		return nil, ErrScope
	}
	return d, nil
}

func (t Target) Verify() error {
	d, err := t.open()
	if err == nil {
		err = d.Close()
	}
	return err
}

type Stage struct {
	Archive Archive
	target  Target
	root    *os.File
	name    string
}

func (s *Stage) Close() {
	_ = unix.Unlinkat(int(s.root.Fd()), s.name, 0)
	_ = s.root.Close()
}

func (s *Stage) Publish() error {
	if err := s.target.Verify(); err != nil {
		return err
	}
	fd := int(s.root.Fd())
	if err := unix.Linkat(fd, s.name, fd, s.Archive.Name, 0); err != nil {
		return err
	}
	return s.root.Sync()
}

func Build(ctx context.Context, src Sources, target Target, id string, now time.Time, full bool) (*Stage, error) {
	name := "bk_" + now.UTC().Format("20060102150405") + "_" + id + ".zip"
	if !archiveName.MatchString(name) {
		return nil, ErrArchive
	}
	root, err := target.open()
	if err != nil {
		return nil, err
	}
	stage := &Stage{target: target, root: root, name: "." + name + ".partial"}
	file, err := openAt(root, stage.name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		root.Close()
		return nil, err
	}
	hash := sha256.New()
	err = writeZIP(ctx, io.MultiWriter(file, hash), src, full)
	if err == nil {
		err = file.Sync()
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if err = errors.Join(err, statErr, closeErr); err != nil {
		stage.Close()
		return nil, err
	}
	stage.Archive = Archive{name, info.Size(), hex.EncodeToString(hash.Sum(nil))}
	return stage, nil
}

func openAt(root *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func writeZIP(ctx context.Context, out io.Writer, src Sources, full bool) error {
	dir, err := os.MkdirTemp("", "nastool-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "user.db")
	if err := sqliteSnapshot(ctx, src.Database, snapshot, full); err != nil {
		return err
	}
	z := zip.NewWriter(out)
	for _, entry := range []struct{ name, path, fallback string }{
		{"config.yaml", src.Config, ""},
		{"default-category.yaml", src.Category, src.FallbackCategory},
		{"user.db", snapshot, ""},
	} {
		f, err := os.Open(entry.path)
		if errors.Is(err, os.ErrNotExist) && entry.fallback != "" {
			f, err = os.Open(entry.fallback)
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return ErrArchive
		}
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(0600)
		w, err := z.CreateHeader(header)
		if err == nil {
			_, err = io.Copy(w, contextReader{ctx, f})
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return z.Close()
}

func sqliteSnapshot(ctx context.Context, src, dest string, full bool) error {
	db, err := sql.Open("sqlite", sqliteDSN(src, "ro"))
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err == nil {
		err = conn.Raw(func(driver any) (err error) {
			online, ok := driver.(interface {
				NewBackup(string) (*sqlite.Backup, error)
			})
			if !ok {
				return ErrArchive
			}
			backup, err := online.NewBackup(sqliteDSN(dest, "rwc"))
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, backup.Finish()) }()
			for more := true; more; {
				if err := ctx.Err(); err != nil {
					return err
				}
				more, err = backup.Step(256)
				if err != nil {
					return err
				}
			}
			return nil
		})
		conn.Close()
	}
	db.Close()
	if err != nil {
		return err
	}
	db, err = sql.Open("sqlite", sqliteDSN(dest, "rw"))
	if err != nil {
		return err
	}
	defer db.Close()
	if !full {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "PRAGMA secure_delete=ON"); err != nil {
			return err
		}
		for _, name := range []string{"SEARCH_RESULT_INFO", "RSS_TORRENTS", "DOUBAN_MEDIAS", "TRANSFER_HISTORY", "TRANSFER_UNKNOWN", "TRANSFER_BLACKLIST", "SYNC_HISTORY", "DOWNLOAD_HISTORY", "alembic_version"} {
			if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return ErrArchive
	}
	return nil
}

func verifyAt(ctx context.Context, root *os.File, a Archive) (*os.File, error) {
	if !archiveName.MatchString(a.Name) || a.Size <= 0 || len(a.Digest) != 64 {
		return nil, ErrArchive
	}
	f, err := openAt(root, a.Name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != a.Size || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, ErrArchive
	}
	hash := sha256.New()
	n, err := io.Copy(hash, contextReader{ctx, f})
	if err != nil || n != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.Digest {
		f.Close()
		return nil, ErrArchive
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func Open(ctx context.Context, target Target, a Archive) (*os.File, error) {
	root, err := target.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return verifyAt(ctx, root, a)
}

func Remove(ctx context.Context, target Target, a Archive, admit func(func() error) error) error {
	root, err := target.open()
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := verifyAt(ctx, root, a)
	if err != nil {
		return err
	}
	defer f.Close()
	return admit(func() error {
		if err := target.Verify(); err != nil {
			return err
		}
		var leaf, opened unix.Stat_t
		if unix.Fstatat(int(root.Fd()), a.Name, &leaf, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			unix.Fstat(int(f.Fd()), &opened) != nil || leaf.Dev != opened.Dev || leaf.Ino != opened.Ino {
			return ErrArchive
		}
		if err := unix.Unlinkat(int(root.Fd()), a.Name, 0); err != nil {
			return err
		}
		return root.Sync()
	})
}

func (t Target) Absent(name string) (bool, error) {
	if !archiveName.MatchString(name) {
		return false, ErrArchive
	}
	root, err := t.open()
	if err != nil {
		return false, err
	}
	defer root.Close()
	var info unix.Stat_t
	err = unix.Fstatat(int(root.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if err := root.Sync(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}
