package autosub

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

//go:embed ct2_models.json
var ct2Manifest []byte

type modelObject struct {
	Bytes int64
	Hash  string
}
type modelRevision struct {
	Revision string
	Files    map[string]modelObject
}

var ct2Models = func() map[string]modelRevision {
	var result map[string]modelRevision
	if err := json.Unmarshal(ct2Manifest, &result); err != nil {
		panic(err)
	}
	return result
}()
var revisionPattern = regexp.MustCompile("^[a-f0-9]{40}$")
var errModel = errors.New("CTranslate2 模型缺失、不可读取或校验失败")

type FasterWhisper struct{ Model, Directory string }

func FasterModels() []string {
	return []string{"tiny", "tiny.en", "base", "base.en", "small", "small.en", "medium", "medium.en", "large-v1", "large-v2"}
}

type modelFile struct {
	name string
	file *os.File
}
type modelFiles []modelFile

func (m modelFiles) close() {
	for _, f := range m {
		f.file.Close()
	}
}

func regularAt(root *os.Root, path string, limit int64) (*os.File, error) {
	// Check the type before opening: old cache directories can contain FIFOs.
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errModel
	}
	f, err := openModelFile(root, path)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || info.Size() != actual.Size() {
		f.Close()
		return nil, errModel
	}
	return f, nil
}
func openSnapshot(root *os.Root, path string) (modelFiles, error) {
	var files modelFiles
	for _, name := range []string{"model.bin", "config.json", "vocabulary.json", "vocabulary.txt"} {
		limit := int64(64 << 20)
		if name == "model.bin" {
			limit = 12 << 30
		}
		f, err := regularAt(root, filepath.Join(path, name), limit)
		if name == "vocabulary.json" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			files.close()
			return nil, err
		}
		files = append(files, modelFile{name, f})
		if strings.HasPrefix(name, "vocabulary.") {
			break
		}
	}
	return files, nil
}
func readRevision(root *os.Root, path string) (string, error) {
	f, err := regularAt(root, path, 128)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 129))
	ref := strings.TrimSpace(string(b))
	if err != nil || !revisionPattern.MatchString(ref) {
		return "", errModel
	}
	return ref, nil
}
func defaultHubCache() string {
	for _, name := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE"} {
		if path := os.Getenv(name); filepath.IsAbs(path) {
			return path
		}
	}
	if path := os.Getenv("HF_HOME"); filepath.IsAbs(path) {
		return filepath.Join(path, "hub")
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(cache) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "huggingface", "hub")
}
func cachedSnapshot(root *os.Root, model string, prefixes []string) (modelFiles, error) {
	for _, prefix := range prefixes {
		for _, owner := range []string{"Systran", "guillaumekln"} {
			repo := filepath.Join(prefix, "models--"+owner+"--faster-whisper-"+model)
			ref, e := readRevision(root, filepath.Join(repo, "refs", "main"))
			if errors.Is(e, os.ErrNotExist) {
				ref = ct2Models[model].Revision
			} else if e != nil {
				return nil, e
			}
			files, e := openSnapshot(root, filepath.Join(repo, "snapshots", ref))
			if !errors.Is(e, os.ErrNotExist) {
				return files, e
			}
		}
	}
	return nil, os.ErrNotExist
}
func (m FasterWhisper) resolve(ctx context.Context, client *http.Client) (modelFiles, error) {
	manifest, ok := ct2Models[m.Model]
	if !ok || !filepath.IsAbs(m.Directory) {
		return nil, errModel
	}
	root, err := os.OpenRoot(m.Directory)
	if err != nil {
		return nil, errModel
	}
	defer root.Close()
	files, e := cachedSnapshot(root, m.Model, []string{"cache", ""})
	if e == nil {
		return files, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return nil, errModel
	}
	// An explicitly supplied extracted model also works offline.
	for _, folder := range []string{m.Model, "."} {
		if _, e := root.Lstat(filepath.Join(folder, "model.bin")); errors.Is(e, os.ErrNotExist) {
			continue
		} else if e != nil {
			return nil, errModel
		}
		return openSnapshot(root, folder)
	}
	// huggingface_hub can initialize its default cache before the legacy
	// plugin sets HUGGINGFACE_HUB_CACHE. Accept those snapshots read-only.
	if shared, err := os.OpenRoot(defaultHubCache()); err == nil {
		files, e := cachedSnapshot(shared, m.Model, []string{""})
		shared.Close()
		if e == nil {
			return files, nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return nil, errModel
		}
	}
	dir := "."
	for _, part := range []string{"cache", "nastool-ct2", m.Model, manifest.Revision} {
		dir = filepath.Join(dir, part)
		if e := root.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
			return nil, errModel
		}
	}
	cache, err := root.OpenRoot(dir)
	if err != nil {
		return nil, errModel
	}
	defer cache.Close()
	names := []string{}
	for name := range manifest.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		obj := manifest.Files[name]
		address := "https://huggingface.co/Systran/faster-whisper-" + m.Model + "/resolve/" + manifest.Revision + "/" + name
		if e := downloadObject(ctx, client, cache, name, address, obj); e != nil {
			return nil, e
		}
	}
	return openSnapshot(cache, ".")
}
func verifyObject(ctx context.Context, f *os.File, obj modelObject) error {
	if _, e := f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	var digest hash.Hash
	if len(obj.Hash) == 64 {
		digest = sha256.New()
	} else {
		digest = sha1.New()
		fmt.Fprintf(digest, "blob %d%c", obj.Bytes, 0)
	}
	buf := make([]byte, 256<<10)
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, e := f.Read(buf)
		digest.Write(buf[:n])
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
	}
	if hex.EncodeToString(digest.Sum(nil)) != obj.Hash {
		return errModel
	}
	return nil
}
func downloadObject(ctx context.Context, client *http.Client, root *os.Root, name, address string, obj modelObject) error {
	lock, e := lockModelObject(ctx, root, "."+name+".lock")
	if e != nil {
		return e
	}
	defer lock.Close()
	tmp := "." + name + ".part"
	if f, e := regularAt(root, name, obj.Bytes); e == nil {
		defer f.Close()
		if e := verifyObject(ctx, f, obj); e != nil {
			return e
		}
		// A crash between link and unlink leaves two names for our verified file.
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if part, e := root.Lstat(tmp); e == nil && os.SameFile(info, part) {
			return root.Remove(tmp)
		}
		return nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return errModel
	}
	f, e := openModelPart(root, tmp)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	offset := info.Size()
	if offset > obj.Bytes {
		return errModel
	}
	if offset < obj.Bytes {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if e != nil {
			return errModel
		}
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, e := client.Do(req)
		if e != nil {
			return errors.New("无法下载公开的 CTranslate2 模型，请检查网络或使用已有模型目录")
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
			offset = 0
		case http.StatusPartialContent:
			if res.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, obj.Bytes-1, obj.Bytes) {
				return errModel
			}
		default:
			return fmt.Errorf("CTranslate2 模型下载返回 HTTP %d", res.StatusCode)
		}
		remaining := obj.Bytes - offset
		if res.ContentLength >= 0 && res.ContentLength != remaining {
			return errModel
		}
		if _, e = f.Seek(offset, io.SeekStart); e != nil {
			return e
		}
		if e = f.Truncate(offset); e != nil {
			return e
		}
		n, e := io.Copy(f, io.LimitReader(res.Body, remaining+1))
		if n > remaining {
			_ = f.Truncate(0)
			return errModel
		}
		if e != nil || n != remaining {
			return errModel
		}
	}
	if e = verifyObject(ctx, f, obj); e != nil {
		if errors.Is(e, errModel) {
			_ = f.Truncate(0)
		}
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	return publishModelObject(root, tmp, name, f)
}

var modelClient = &http.Client{Timeout: 45 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
	if r.URL.Scheme != "https" || len(via) > 10 {
		return errModel
	}
	return nil
}}
