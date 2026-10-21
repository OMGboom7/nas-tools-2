// Package remotestore executes a bounded, credential-isolated Rclone protocol.
// A remote namespace is not a local inode namespace: callers must obtain
// explicit exclusive-writer/non-atomic publication and recovery-copy consent.
package remotestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrUnavailable = errors.New("remote configuration or bounded Rclone operation unavailable")
var ErrChanged = errors.New("reviewed remote configuration or content changed")
var ErrLimit = errors.New("remote inventory or response limit exceeded")

type Binding struct {
	Driver, ConfigFile, ConfigDigest, Binary, BinaryVersion, Environment string
	Directories                                                          bool
	Protocol                                                             int
}
type Summary struct {
	Driver       string `json:"driver"`
	Atomic       bool   `json:"atomic"`
	RecoveryCopy bool   `json:"recoveryCopy"`
	Directories  bool   `json:"directories"`
	Evidence     string `json:"evidence"`
}

func (b Binding) Summary() *Summary {
	return &Summary{Driver: b.Driver, RecoveryCopy: true, Directories: b.Directories, Evidence: "exclusive-namespace/full-content"}
}

type Factory struct {
	ConfigFile, Binary string
	Environment        []string
}
type Client struct {
	factory     Factory
	binding     Binding
	dir, config string
	environment []string
	dirInfo     os.FileInfo
}
type Node struct {
	Path, Name, ID, ModTime string
	Size                    int64
	IsDir                   bool
}
type Features struct {
	Features *struct{ CanHaveEmptyDirectories bool }
}

func digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func key(s string) string { return cases.Fold().String(norm.NFD.String(s)) }

var executableDigests sync.Map

// Persist content, not an ephemeral device/inode: identical container rebuilds
// or reinstalling the same reviewed CLI must not orphan durable job receipts.
// A local ctime/device/inode stamp is used only to cache full executable hashes
// within this process; a changed binary is rehashed before any command.
func executableVersion(name string, before os.FileInfo) (string, error) {
	stamp, err := binaryChangeStamp(name)
	if err != nil {
		return "", err
	}
	cacheKey := digest([]any{name, stamp, before.Size(), before.ModTime().UnixNano(), before.Mode()})
	if stamp != "" {
		if saved, ok := executableDigests.Load(cacheKey); ok {
			return saved.(string), nil
		}
	}
	f, err := os.Open(name)
	if err != nil {
		return "", ErrUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != before.Size() || opened.Size() < 1 || opened.Size() > 512<<20 {
		return "", ErrChanged
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (512<<20)+1))
	after, statErr := os.Stat(name)
	endStamp, stampErr := binaryChangeStamp(name)
	if err != nil || statErr != nil || stampErr != nil || n != before.Size() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || stamp != endStamp {
		return "", ErrChanged
	}
	version := hex.EncodeToString(h.Sum(nil))
	if stamp != "" {
		executableDigests.Store(cacheKey, version)
	}
	return version, nil
}
func relative(s string) bool {
	return s != "" && s != "." && !path.IsAbs(s) && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "\\\x00\r\n")
}
func safeRoot(s string) bool {
	return path.IsAbs(s) && s != "/" && path.Clean(s) == s && !strings.ContainsAny(s, "\\\x00\r\n")
}
func address(s string) (string, error) {
	if !safeRoot(s) {
		return "", ErrUnavailable
	}
	return "NASTOOL:" + s, nil
}
func (f Factory) base() (Binding, []byte, []string, error) {
	if !filepath.IsAbs(f.ConfigFile) || filepath.Clean(f.ConfigFile) == "/" {
		return Binding{}, nil, nil, ErrUnavailable
	}
	configFile, err := filepath.EvalSymlinks(f.ConfigFile)
	if err != nil {
		return Binding{}, nil, nil, ErrUnavailable
	}
	file, err := os.Open(configFile)
	if err != nil {
		return Binding{}, nil, nil, ErrUnavailable
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 4<<20 {
		return Binding{}, nil, nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return Binding{}, nil, nil, ErrUnavailable
	}
	after, err := file.Stat()
	named, nameErr := os.Stat(configFile)
	if err != nil || nameErr != nil || !os.SameFile(before, after) || !os.SameFile(after, named) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return Binding{}, nil, nil, ErrChanged
	}
	// Do not launch operator password/SSH helpers, which may themselves depend
	// on Python or a shell. Encrypted files cannot be inspected for such helpers
	// before execution, so require an explicitly selected inspectable snapshot.
	lower := strings.ToLower(string(data))
	if strings.HasPrefix(strings.TrimSpace(string(data)), "RCLONE_ENCRYPT_") || strings.Contains(lower, "password_command") || strings.Contains(lower, "pass_command") {
		return Binding{}, nil, nil, ErrUnavailable
	}
	for _, line := range strings.Split(lower, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(name) == "ssh" && strings.TrimSpace(value) != "" {
			return Binding{}, nil, nil, ErrUnavailable
		}
	}
	env := f.Environment
	if env == nil {
		env = os.Environ()
	}
	filtered := []string{}
	for _, line := range env {
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, "RCLONE_CONFIG_") && (strings.HasSuffix(name, "_PASS_COMMAND") || strings.HasSuffix(name, "_PASSWORD_COMMAND") || strings.HasSuffix(name, "_SSH")) && strings.TrimPrefix(line, name+"=") != "" {
			return Binding{}, nil, nil, ErrUnavailable
		}
		if strings.HasPrefix(name, "RCLONE_CONFIG_") && name != "RCLONE_CONFIG_PASS_COMMAND" {
			filtered = append(filtered, line)
			continue
		}
		if name == "PATH" || name == "HOME" || name == "TMPDIR" || name == "SYSTEMROOT" || name == "SSL_CERT_FILE" || name == "SSL_CERT_DIR" || strings.EqualFold(name, "http_proxy") || strings.EqualFold(name, "https_proxy") || strings.EqualFold(name, "all_proxy") || strings.EqualFold(name, "no_proxy") {
			filtered = append(filtered, line)
		}
	}
	sort.Strings(filtered)
	if len(strings.Join(filtered, "\n")) > 256<<10 {
		return Binding{}, nil, nil, ErrLimit
	}
	if !strings.Contains(string(data), "[NASTOOL]") {
		return Binding{}, nil, nil, ErrUnavailable
	}
	binary := f.Binary
	if binary == "" {
		binary = "rclone"
	}
	binary, err = exec.LookPath(binary)
	if err != nil {
		return Binding{}, nil, nil, ErrUnavailable
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return Binding{}, nil, nil, ErrUnavailable
	}
	bi, err := os.Stat(binary)
	if err != nil || !bi.Mode().IsRegular() || bi.Mode().Perm()&0111 == 0 {
		return Binding{}, nil, nil, ErrUnavailable
	}
	version, err := executableVersion(binary, bi)
	if err != nil {
		return Binding{}, nil, nil, err
	}
	h := sha256.Sum256(data)
	b := Binding{Driver: "rclone", ConfigFile: configFile, ConfigDigest: hex.EncodeToString(h[:]), Binary: binary, BinaryVersion: version, Environment: digest(filtered), Protocol: 1}
	return b, data, filtered, nil
}
func (f Factory) Available() bool { _, _, _, err := f.base(); return err == nil }
func (f Factory) Validate(expected Binding) error {
	b, _, _, err := f.base()
	if err != nil {
		return err
	}
	b.Directories = expected.Directories
	if b != expected {
		return ErrChanged
	}
	return nil
}
func (f Factory) Open(expected *Binding) (*Client, error) {
	b, data, env, err := f.base()
	if err != nil {
		return nil, err
	}
	if expected != nil {
		actual := b
		actual.Directories = expected.Directories
		if actual != *expected {
			return nil, ErrChanged
		}
		b = actual
	}
	dir, err := os.MkdirTemp("", "nastool-rclone-")
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := os.Chmod(dir, 0700); err != nil {
		os.Remove(dir)
		return nil, ErrUnavailable
	}
	configFile := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		os.RemoveAll(dir)
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, ErrUnavailable
	}
	return &Client{factory: f, binding: b, dir: dir, config: configFile, environment: env, dirInfo: info}, nil
}
func (c *Client) Close() error {
	info, err := os.Lstat(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || c.dirInfo == nil || !info.IsDir() || !os.SameFile(info, c.dirInfo) {
		return ErrChanged
	}
	return os.RemoveAll(c.dir)
}
func (c *Client) Binding() Binding { return c.binding }
func (c *Client) Current() error {
	b, _, _, err := c.factory.base()
	if err != nil {
		return err
	}
	b.Directories = c.binding.Directories
	if b != c.binding {
		return ErrChanged
	}
	return nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
	cancel    context.CancelFunc
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.cancel()
		return 0, ErrLimit
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
func (c *Client) command(ctx context.Context, args []string, input io.Reader, output io.Writer, limit int64) error {
	if err := c.Current(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	flags := []string{"--config", c.config, "--retries", "1", "--low-level-retries", "1", "--stats", "0", "--log-level", "ERROR", "--buffer-size", "0", "--temp-dir", c.dir}
	cmd := exec.CommandContext(ctx, c.binding.Binary, append(flags, args...)...)
	cmd.Env = c.environment
	cmd.Stdin = input
	cmd.Stdout = &limitedWriter{writer: output, remaining: limit, cancel: cancel}
	cmd.Stderr = &limitedWriter{writer: io.Discard, remaining: 64 << 10, cancel: cancel}
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var e *exec.ExitError
		if errors.As(err, &e) {
			return remoteExit(e.ExitCode())
		}
		return ErrUnavailable
	}
	return c.Current()
}

type remoteExit int

func (remoteExit) Error() string {
	return "bounded remote command failed; credentials and command output withheld"
}
func (remoteExit) Unwrap() error { return ErrUnavailable }
func missing(err error) bool     { var e remoteExit; return errors.As(err, &e) && (e == 3 || e == 4) }
func (c *Client) Probe(ctx context.Context, root string) error {
	a, err := address(root)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := c.command(ctx, []string{"backend", "features", a}, nil, &b, 1<<20); err != nil {
		return err
	}
	var f Features
	if json.Unmarshal(b.Bytes(), &f) != nil || f.Features == nil {
		return ErrUnavailable
	}
	c.binding.Directories = f.Features.CanHaveEmptyDirectories
	_, err = c.List(ctx, root, false)
	return err
}
func (c *Client) List(ctx context.Context, location string, recursive bool) ([]Node, error) {
	a, err := address(location)
	if err != nil {
		return nil, err
	}
	args := []string{"lsjson", a, "--links", "--no-mimetype"}
	if recursive {
		args = append(args, "--recursive", "--max-depth", "65")
	} else {
		args = append(args, "--max-depth", "1")
	}
	var b bytes.Buffer
	if err := c.command(ctx, args, nil, &b, 16<<20); err != nil {
		return nil, err
	}
	var nodes []Node
	if json.Unmarshal(b.Bytes(), &nodes) != nil || len(nodes) > 20001 {
		return nil, ErrLimit
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if !relative(n.Path) || n.Size < 0 && !n.IsDir || seen[key(n.Path)] || strings.Contains(n.Path, ".rclonelink") {
			return nil, ErrChanged
		}
		seen[key(n.Path)] = true
		if !recursive && path.Base(n.Path) != n.Path {
			return nil, ErrChanged
		}
	}
	return nodes, nil
}

// Entry walks ancestors from the configured logical root. Listings expose
// filesystem symlinks through --links; they are never accepted as directories.
func (c *Client) Entry(ctx context.Context, root, relativePath string) (*Node, error) {
	return c.entry(ctx, root, relativePath, false)
}

// Existing is only for non-mutating preservation decisions: a case/Unicode
// equivalent leaf is enough to leave an existing object alone. Ancestors still
// require exact names, and ambiguous inventories or links remain errors.
func (c *Client) Existing(ctx context.Context, root, relativePath string) (bool, error) {
	n, err := c.entry(ctx, root, relativePath, true)
	return n != nil, err
}
func (c *Client) entry(ctx context.Context, root, relativePath string, equivalentLeaf bool) (*Node, error) {
	if !safeRoot(root) || !relative(relativePath) {
		return nil, ErrUnavailable
	}
	parts := strings.Split(relativePath, "/")
	parent := root
	for i, name := range parts {
		nodes, err := c.List(ctx, parent, false)
		if missing(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var found *Node
		for _, n := range nodes {
			if key(n.Path) == key(name) {
				if n.Path != name && !(equivalentLeaf && i == len(parts)-1) {
					return nil, ErrChanged
				}
				v := n
				found = &v
			}
		}
		if found == nil {
			return nil, nil
		}
		if i == len(parts)-1 {
			return found, nil
		}
		if !found.IsDir {
			return nil, ErrChanged
		}
		parent = path.Join(parent, name)
	}
	return nil, ErrChanged
}
func (c *Client) Read(ctx context.Context, location string, size int64, out io.Writer) error {
	if size < 0 || size > 1<<50 {
		return ErrLimit
	}
	a, err := address(location)
	if err != nil {
		return err
	}
	return c.command(ctx, []string{"cat", a, "--count", strconv.FormatInt(size+1, 10)}, nil, out, size+1)
}
func (c *Client) Upload(ctx context.Context, location string, size int64, input io.Reader) error {
	if size < 0 || size > 1<<50 {
		return ErrLimit
	}
	a, err := address(location)
	if err != nil {
		return err
	}
	return c.command(ctx, []string{"rcat", a, "--size", strconv.FormatInt(size, 10)}, input, io.Discard, 1<<20)
}
func (c *Client) Copy(ctx context.Context, from, to string) error {
	a, err := address(from)
	if err != nil {
		return err
	}
	b, err := address(to)
	if err != nil {
		return err
	}
	// Some Rclone versions return success after skipping an immutable conflict.
	// Never use process status to infer a new publication, or ask copyto to
	// overwrite any existing object. The exclusive-writer contract still
	// matters: generic remote APIs do not offer atomic no-replace publication.
	if n, err := c.Entry(ctx, path.Dir(to), path.Base(to)); err != nil {
		return err
	} else if n != nil {
		return ErrChanged
	}
	return c.command(ctx, []string{"copyto", a, b, "--immutable", "--checksum", "--no-update-modtime"}, nil, io.Discard, 1<<20)
}
func (c *Client) Mkdir(ctx context.Context, location string) error {
	if !c.binding.Directories {
		return nil
	}
	a, err := address(location)
	if err != nil {
		return err
	}
	return c.command(ctx, []string{"mkdir", a}, nil, io.Discard, 1<<20)
}
