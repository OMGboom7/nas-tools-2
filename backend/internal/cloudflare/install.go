package cloudflare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrDispatch = errors.New("Cloudflare action intent was not authorized")

type Operation struct{ Kind, Target, Digest string }
type Before func(context.Context, Operation) error

func permit(ctx context.Context, before Before, op Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if before == nil {
		return ErrDispatch
	}
	if err := before(ctx, op); err != nil {
		return errors.Join(ErrDispatch, err)
	}
	return nil
}

type Installation struct {
	Root      string            `json:"-"`
	Directory string            `json:"directory"`
	Binary    string            `json:"binary"`
	Version   string            `json:"version"`
	Digest    string            `json:"digest"`
	Files     map[string]string `json:"files"`
	Warning   string            `json:"warning,omitempty"`
}

type Installer struct {
	Root     string
	Client   *http.Client
	OS, Arch string
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}
type release struct {
	Version string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

func sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (i Installer) platform() (string, string, error) {
	osName, arch := i.OS, i.Arch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName != "linux" && osName != "darwin" {
		return "", "", ErrState
	}
	if arch != "amd64" && arch != "arm64" && !(osName == "linux" && (arch == "386" || arch == "arm")) {
		return "", "", ErrState
	}
	return osName, arch, nil
}

func allowedDownload(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" && u.Port() != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com"
}
func (i Installer) fetch(ctx context.Context, target string, limit int64, before Before) ([]byte, error) {
	u, err := url.Parse(target)
	if err != nil || !allowedDownload(u) && !(u.Scheme == "https" && u.Host == "api.github.com" && u.User == nil && u.Fragment == "") {
		return nil, ErrState
	}
	if err := permit(ctx, before, Operation{Kind: "download", Target: target}); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 90 * time.Second}
	if i.Client != nil {
		client = *i.Client
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !allowedDownload(r.URL) {
			return ErrState
		}
		if previous != nil {
			return previous(r, via)
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "NAS-Tools-Go-Cloudflare")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > limit {
		return nil, ErrResult
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrResult
	}
	return data, nil
}

func selectAsset(r release, osName, arch string) (releaseAsset, error) {
	if !versionPattern.MatchString(r.Version) || len(r.Assets) > 128 {
		return releaseAsset{}, ErrState
	}
	if arch == "arm" {
		arch = "armv7"
	}
	extension := ".tar.gz"
	if osName == "darwin" {
		extension = ".zip"
	}
	names := map[string]bool{"CloudflareST_" + osName + "_" + arch + extension: true, "cfst_" + osName + "_" + arch + extension: true}
	var selected releaseAsset
	for _, asset := range r.Assets {
		if !names[asset.Name] {
			continue
		}
		u, err := url.Parse(asset.URL)
		expected := "/XIU2/CloudflareSpeedTest/releases/download/" + r.Version + "/" + asset.Name
		if err != nil || !allowedDownload(u) || u.Hostname() != "github.com" || !strings.EqualFold(u.Path, expected) || u.RawQuery != "" || selected.Name != "" {
			return releaseAsset{}, ErrState
		}
		if asset.Digest != "" {
			if !strings.HasPrefix(asset.Digest, "sha256:") {
				return releaseAsset{}, ErrState
			}
			digest, e := hex.DecodeString(strings.TrimPrefix(asset.Digest, "sha256:"))
			if e != nil || len(digest) != 32 {
				return releaseAsset{}, ErrState
			}
		}
		selected = asset
	}
	if selected.Name == "" {
		return selected, ErrState
	}
	return selected, nil
}

func (i Installer) latest(ctx context.Context, suffix string, before Before) (release, error) {
	data, err := i.fetch(ctx, "https://api.github.com/repos/XIU2/CloudflareSpeedTest/releases/"+suffix, 2<<20, before)
	if err != nil {
		return release{}, err
	}
	var r release
	if json.Unmarshal(data, &r) != nil || !versionPattern.MatchString(r.Version) {
		return r, ErrResult
	}
	return r, nil
}

func regular(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, ErrState
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrState
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, ErrState
	}
	return data, nil
}

func validateInstall(in Installation, s Settings, osName, arch string) error {
	if in.Root == "" || !filepath.IsAbs(in.Root) || in.Directory == "" || filepath.Clean(in.Directory) != in.Directory || !filepath.IsLocal(in.Directory) && in.Directory != "." {
		return ErrState
	}
	if in.Directory != "." {
		parts := strings.Split(in.Directory, string(filepath.Separator))
		if len(parts) != 2 || parts[0] != ".go-versions" {
			return ErrState
		}
		parent, err := os.Lstat(filepath.Join(in.Root, ".go-versions"))
		if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return ErrState
		}
	}
	directory := filepath.Join(in.Root, in.Directory)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrState
	}
	if in.Binary != "CloudflareST" && in.Binary != "cfst" {
		return ErrState
	}
	for name, digest := range in.Files {
		if name != in.Binary && name != "ip.txt" && name != "ipv6.txt" {
			return ErrState
		}
		limit := int64(2 << 20)
		if name == in.Binary {
			limit = 64 << 20
		}
		data, err := regular(filepath.Join(directory, name), limit)
		if err != nil || sum(data) != digest {
			return ErrState
		}
		if name == in.Binary && nativeExecutable(data, osName, arch) != nil {
			return ErrState
		}
		if name == in.Binary {
			info, err := os.Lstat(filepath.Join(directory, name))
			if err != nil || info.Mode().Perm()&0111 == 0 {
				return ErrState
			}
		}
	}
	if in.Files[in.Binary] == "" || in.Digest != in.Files[in.Binary] || s.IPv4 && in.Files["ip.txt"] == "" || s.IPv6 && in.Files["ipv6.txt"] == "" {
		return ErrState
	}
	return nil
}

func (i Installer) current(s Settings, osName, arch string) (Installation, error) {
	if data, err := regular(filepath.Join(i.Root, ".go-install.json"), 16<<10); err == nil {
		var in Installation
		if json.Unmarshal(data, &in) != nil || !versionPattern.MatchString(in.Version) || !strings.HasPrefix(in.Directory, ".go-versions"+string(filepath.Separator)) {
			return in, ErrState
		}
		in.Root = i.Root
		if validateInstall(in, s, osName, arch) != nil {
			return in, ErrState
		}
		return in, nil
	}
	// Original local files are never deleted or reinterpreted as a script.
	for _, name := range []string{"CloudflareST", "cfst"} {
		in := Installation{Root: i.Root, Directory: ".", Binary: name, Version: s.Version, Files: map[string]string{}}
		if in.Version == "" {
			in.Version = "v2.2.2"
		}
		for _, file := range []string{name, "ip.txt", "ipv6.txt"} {
			if data, err := regular(filepath.Join(i.Root, file), 64<<20); err == nil {
				in.Files[file] = sum(data)
			}
		}
		in.Digest = in.Files[name]
		if validateInstall(in, s, osName, arch) == nil {
			return in, nil
		}
	}
	return Installation{}, ErrState
}

func plainDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrState
	}
	return nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func (i Installer) publish(ctx context.Context, s Settings, r release, files map[string][]byte, before Before) (Installation, error) {
	name := "CloudflareST"
	if files[name] == nil {
		name = "cfst"
	}
	in := Installation{Root: i.Root, Binary: name, Version: r.Version, Digest: sum(files[name]), Files: map[string]string{}}
	in.Directory = filepath.Join(".go-versions", r.Version+"-"+in.Digest[:16])
	for name, data := range files {
		in.Files[name] = sum(data)
	}
	if err := permit(ctx, before, Operation{Kind: "install", Target: r.Version, Digest: sumJSON(in)}); err != nil {
		return in, err
	}
	if err := plainDirectory(i.Root); err != nil {
		return in, err
	}
	parent := filepath.Join(i.Root, ".go-versions")
	if err := plainDirectory(parent); err != nil {
		return in, err
	}
	stage, err := os.MkdirTemp(parent, ".install-")
	if err != nil {
		return in, err
	}
	defer os.RemoveAll(stage)
	for filename, data := range files {
		mode := os.FileMode(0600)
		if filename == name {
			mode = 0700
		}
		if err := writeNew(filepath.Join(stage, filename), data, mode); err != nil {
			return in, err
		}
	}
	osName, arch, _ := i.platform()
	staged := in
	staged.Directory, _ = filepath.Rel(i.Root, stage)
	if err := validateInstall(staged, s, osName, arch); err != nil {
		return in, err
	}
	target := filepath.Join(i.Root, in.Directory)
	if _, err := os.Lstat(target); err == nil {
		if err := validateInstall(in, s, osName, arch); err != nil {
			return in, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return in, err
	} else if err := os.Rename(stage, target); err != nil {
		return in, err
	}
	data, _ := json.Marshal(in)
	f, err := os.CreateTemp(i.Root, ".install-manifest-")
	if err != nil {
		return in, err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return in, err
	}
	if err := ctx.Err(); err != nil {
		return in, err
	}
	if err := os.Rename(temporary, filepath.Join(i.Root, ".go-install.json")); err != nil {
		return in, err
	}
	return in, nil
}

func sumJSON(v any) string { data, _ := json.Marshal(v); return sum(data) }

func (i Installer) Ensure(ctx context.Context, s Settings, before Before) (Installation, error) {
	if !filepath.IsAbs(i.Root) || filepath.Clean(i.Root) == string(filepath.Separator) {
		return Installation{}, ErrState
	}
	osName, arch, err := i.platform()
	if err != nil {
		return Installation{}, err
	}
	current, currentErr := i.current(s, osName, arch)
	fallback := func(err error) (Installation, error) {
		if ctx.Err() != nil || errors.Is(err, ErrDispatch) {
			return Installation{}, err
		}
		if currentErr == nil {
			current.Warning = "更新不可用，保留原可用安装"
			return current, nil
		}
		return Installation{}, err
	}
	r, err := i.latest(ctx, "latest", before)
	if err != nil {
		if currentErr == nil || ctx.Err() != nil || errors.Is(err, ErrDispatch) {
			return fallback(err)
		}
		version := s.Version
		if version == "" {
			version = "v2.2.2"
		}
		r, err = i.latest(ctx, "tags/"+version, before)
		if err != nil {
			return fallback(err)
		}
	}
	if currentErr == nil && current.Version == r.Version && !s.Reinstall {
		return current, nil
	}
	asset, err := selectAsset(r, osName, arch)
	if err != nil {
		return fallback(err)
	}
	data, err := i.fetch(ctx, asset.URL, 32<<20, before)
	if err != nil {
		return fallback(err)
	}
	if asset.Digest != "" && !strings.EqualFold(asset.Digest, "sha256:"+sum(data)) {
		return fallback(ErrResult)
	}
	files, err := unpack(asset.Name, data, osName, arch)
	if err != nil {
		return fallback(err)
	}
	installed, err := i.publish(ctx, s, r, files, before)
	if err != nil {
		return fallback(fmt.Errorf("install Cloudflare native executable: %w", err))
	}
	return installed, nil
}
