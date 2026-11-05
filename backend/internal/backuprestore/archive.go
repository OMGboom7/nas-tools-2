package backuprestore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

const MaxArchive = 2 << 30
const maxYAML = 4 << 20
const maxCategory = 1 << 20
const maxDatabase = 8 << 30
const stateDir = ".nastool-restore"

var (
	ErrArchive = errors.New("备份归档无效")
	ErrState   = errors.New("原文件、恢复记录或目录已变化")
	ErrBusy    = errors.New("仍有服务或恢复命令使用配置目录")
	ErrHold    = errors.New("恢复需要核对后单独确认启动")
	idPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Stage struct {
	ID      string `json:"id"`
	Created string `json:"created"`
	Files   []File `json:"files"`
}

type Workspace struct {
	root   string
	config string
}

func New(configPath string) (Workspace, error) {
	if configPath == "" {
		return Workspace{}, ErrState
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return Workspace{}, err
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return Workspace{}, err
	}
	name := filepath.Base(abs)
	switch name {
	case "default-category.yaml", "user.db", "user.db-wal", "user.db-shm", "user.db-journal", "user.db.pre-go", "user.db.pre-go.bak", stateDir, ".nastool-restore.lock":
		return Workspace{}, ErrState
	}
	if strings.HasPrefix(name, ".") {
		return Workspace{}, ErrState
	}
	return Workspace{root, name}, nil
}

func (w Workspace) private() string            { return filepath.Join(w.root, stateDir) }
func (w Workspace) candidate(id string) string { return filepath.Join(w.private(), id) }

func privateDir(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !owned(info) {
		return ErrState
	}
	return nil
}

func inspectFile(ctx context.Context, path, name string) (File, error) {
	f, err := openRegular(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, cancelReader{ctx, f})
	if err != nil {
		return File{}, err
	}
	return File{name, n, hex.EncodeToString(h.Sum(nil))}, nil
}

type cancelReader struct {
	context.Context
	io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func writeJSON(path string, value any) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func readJSON(path string, value any) error {
	f, err := openRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return ErrState
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrState
	}
	return nil
}

func (w Workspace) Stage(ctx context.Context, input io.Reader) (Stage, error) {
	lock, err := w.lock(false)
	if err != nil {
		return Stage{}, err
	}
	defer lock.Close()
	upload, err := w.lockFile(".nastool-restore-upload.lock", true)
	if err != nil {
		return Stage{}, err
	}
	defer upload.Close()
	if err := privateDir(w.private(), true); err != nil {
		return Stage{}, err
	}
	if err := syncDir(w.root); err != nil {
		return Stage{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Stage{}, err
	}
	id := hex.EncodeToString(nonce[:])
	path := w.candidate(id)
	if err := os.Mkdir(path, 0700); err != nil {
		return Stage{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(path)
		}
	}()
	incoming := filepath.Join(path, "incoming")
	if err := os.Mkdir(incoming, 0700); err != nil {
		return Stage{}, err
	}
	if err := os.Mkdir(filepath.Join(path, "previous"), 0700); err != nil {
		return Stage{}, err
	}
	file, err := os.OpenFile(filepath.Join(path, "upload.zip"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Stage{}, err
	}
	defer file.Close()
	n, err := io.Copy(file, io.LimitReader(cancelReader{ctx, input}, MaxArchive+1))
	if err != nil {
		return Stage{}, err
	}
	if n > MaxArchive {
		return Stage{}, ErrArchive
	}
	if err := checkDirectory(file, n); err != nil {
		return Stage{}, err
	}
	z, err := zip.NewReader(file, n)
	if err != nil || len(z.File) != 3 {
		return Stage{}, ErrArchive
	}
	limits := map[string]uint64{"config.yaml": maxYAML, "default-category.yaml": maxCategory, "user.db": maxDatabase}
	stage := Stage{ID: id, Created: time.Now().UTC().Format(time.RFC3339)}
	for _, entry := range z.File {
		limit, exists := limits[entry.Name]
		if !exists || !entry.Mode().IsRegular() || entry.Flags&1 != 0 || entry.UncompressedSize64 > limit || entry.UncompressedSize64 == 0 {
			return Stage{}, ErrArchive
		}
		delete(limits, entry.Name)
		item, err := extract(ctx, entry, filepath.Join(incoming, entry.Name), int64(limit))
		if err != nil {
			return Stage{}, err
		}
		stage.Files = append(stage.Files, item)
	}
	if err := validate(ctx, incoming); err != nil {
		return Stage{}, err
	}
	if err := file.Close(); err != nil {
		return Stage{}, err
	}
	if err := os.Remove(file.Name()); err != nil {
		return Stage{}, err
	}
	if err := syncDir(incoming); err != nil {
		return Stage{}, err
	}
	if err := syncDir(filepath.Join(path, "previous")); err != nil {
		return Stage{}, err
	}
	if err := writeJSON(filepath.Join(path, "stage.json"), stage); err != nil {
		return Stage{}, err
	}
	if err := syncDir(w.private()); err != nil {
		return Stage{}, err
	}
	ok = true
	return stage, nil
}

// Limit the amount of central-directory data passed to archive/zip. A backup
// has three entries and its compressed size fits the standard ZIP offsets.
func checkDirectory(file io.ReaderAt, size int64) error {
	const endSize = 22
	offset := max(size-(endSize+65535), 0)
	tail := make([]byte, size-offset)
	if _, err := file.ReadAt(tail, offset); err != nil {
		return ErrArchive
	}
	for i := len(tail) - endSize; i >= 0; i-- {
		end := tail[i:]
		if !bytes.Equal(end[:4], []byte{'P', 'K', 5, 6}) {
			continue
		}
		directorySize := int64(binary.LittleEndian.Uint32(end[12:]))
		directoryOffset := int64(binary.LittleEndian.Uint32(end[16:]))
		if int(binary.LittleEndian.Uint16(end[20:])) != len(end)-endSize ||
			binary.LittleEndian.Uint32(end[4:]) != 0 ||
			binary.LittleEndian.Uint16(end[8:]) != 3 || binary.LittleEndian.Uint16(end[10:]) != 3 ||
			directorySize > 64<<10 || directoryOffset+directorySize != offset+int64(i) {
			return ErrArchive
		}
		return nil
	}
	return ErrArchive
}

func extract(ctx context.Context, entry *zip.File, path string, limit int64) (File, error) {
	source, err := entry.Open()
	if err != nil {
		return File{}, ErrArchive
	}
	defer source.Close()
	dest, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dest, h), io.LimitReader(cancelReader{ctx, source}, limit+1))
	if err == nil && (n > limit || uint64(n) != entry.UncompressedSize64) {
		err = ErrArchive
	}
	if err == nil {
		err = dest.Sync()
	}
	if err = errors.Join(err, dest.Close()); err != nil {
		return File{}, err
	}
	return File{entry.Name, n, hex.EncodeToString(h.Sum(nil))}, nil
}

func validate(ctx context.Context, path string) error {
	if _, err := config.LoadApplication(filepath.Join(path, "config.yaml")); err != nil {
		return fmt.Errorf("%w: 主配置不完整", ErrArchive)
	}
	category, err := os.ReadFile(filepath.Join(path, "default-category.yaml"))
	if err != nil {
		return err
	}
	var y yaml.Node
	if yaml.Unmarshal(category, &y) != nil || len(y.Content) != 1 || y.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%w: 分类规则无效", ErrArchive)
	}
	dbPath := filepath.Join(path, "user.db")
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&immutable=1&_pragma=trusted_schema(OFF)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return ErrArchive
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("%w: SQLite 校验失败", ErrArchive)
	}
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('SYSTEM_DICT','CONFIG_USERS')").Scan(&tables); err != nil || tables != 2 {
		return fmt.Errorf("%w: SQLite 缺少 NAS Tools 核心表", ErrArchive)
	}
	for _, query := range []string{"SELECT ID,TYPE,KEY,VALUE FROM SYSTEM_DICT LIMIT 0", "SELECT ID,NAME,PASSWORD,PRIS FROM CONFIG_USERS LIMIT 0"} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("%w: SQLite 缺少 NAS Tools 核心表", ErrArchive)
		}
		rows.Close()
	}
	return nil
}

func (w Workspace) Receipt(id string) (Stage, error) {
	var stage Stage
	if !idPattern.MatchString(id) {
		return stage, ErrState
	}
	if err := privateDir(w.private(), false); err != nil {
		return stage, err
	}
	if err := privateDir(w.candidate(id), false); err != nil {
		return stage, err
	}
	err := readJSON(filepath.Join(w.candidate(id), "stage.json"), &stage)
	if err != nil {
		return Stage{}, err
	}
	if stage.ID != id || len(stage.Files) != 3 {
		return Stage{}, ErrState
	}
	return stage, nil
}
