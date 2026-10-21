package remotestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The synthetic executable tests only process isolation/failure handling. Real
// command semantics are covered separately with an explicit temporary alias.
func TestMain(m *testing.M) {
	if os.Getenv("RCLONE_CONFIG_NASTOOL_TEST_HELPER") == "yes" {
		switch os.Getenv("RCLONE_CONFIG_NASTOOL_TEST_RESPONSE") {
		case "stderr":
			fmt.Fprint(os.Stderr, "private-token-and-account-path")
			os.Exit(5)
		case "large":
			fmt.Fprint(os.Stdout, strings.Repeat("x", 17<<20))
			os.Exit(0)
		case "wait":
			time.Sleep(time.Minute)
			os.Exit(0)
		case "args":
			_ = json.NewEncoder(os.Stdout).Encode(os.Args[1:])
			os.Exit(0)
		default:
			fmt.Fprint(os.Stdout, os.Getenv("RCLONE_CONFIG_NASTOOL_TEST_JSON"))
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}
func testFactory(t *testing.T, response string) Factory {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(file, []byte("[NASTOOL]\ntype = alias\nremote = /unused\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Factory{ConfigFile: file, Binary: binary, Environment: []string{"RCLONE_CONFIG_NASTOOL_TEST_HELPER=yes", "RCLONE_CONFIG_NASTOOL_TEST_RESPONSE=" + response}}
}
func TestFactoryDisabledAndCredentialSnapshotIsPrivateAndBound(t *testing.T) {
	if (Factory{}).Available() {
		t.Fatal("read ambient account configuration")
	}
	f := testFactory(t, "")
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, spec := range []struct {
		path string
		mode os.FileMode
	}{{c.dir, 0700}, {c.config, 0600}} {
		info, err := os.Stat(spec.path)
		if err != nil || info.Mode().Perm() != spec.mode {
			t.Fatal(spec, info, err)
		}
	}
	if c.config == f.ConfigFile || c.Current() != nil {
		t.Fatal("unfrozen config")
	}
	public, _ := json.Marshal(c.Binding().Summary())
	if bytes.Contains(public, []byte(f.ConfigFile)) || bytes.Contains(public, []byte("ConfigDigest")) {
		t.Fatal(string(public))
	}
	if err := os.WriteFile(f.ConfigFile, []byte("[NASTOOL]\ntype = alias\nremote = /another\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Current(), ErrChanged) {
		t.Fatal("profile switch accepted")
	}
	if _, err := f.Open(&c.binding); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
}

func TestExecutableBindingSurvivesIdenticalReinstallationButRejectsChangedBytes(t *testing.T) {
	f := testFactory(t, "")
	data, err := os.ReadFile(f.Binary)
	if err != nil {
		t.Fatal(err)
	}
	f.Binary = filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(f.Binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.Binary)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := os.Rename(f.Binary, f.Binary+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.Binary, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := c.Current(); err != nil {
		t.Fatal("identical replacement orphaned durable binding", err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(f.Binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.Binary, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := c.Current(); !errors.Is(err, ErrChanged) {
		t.Fatal("changed bytes with preserved size/mtime accepted", err)
	}
}
func TestFactoryRejectsPasswordHelpersAndBindsEnvironment(t *testing.T) {
	f := testFactory(t, "")
	for _, env := range [][]string{{"RCLONE_CONFIG_PASS_COMMAND=python secret.py"}, {"RCLONE_CONFIG_NASTOOL_SSH=python ssh.py"}, {"RCLONE_CONFIG_NASTOOL_PASSWORD_COMMAND=python password.py"}, {strings.Repeat("RCLONE_CONFIG_BIG=x", 30000)}} {
		bad := f
		bad.Environment = env
		if bad.Available() {
			t.Fatal("unsafe environment accepted")
		}
	}
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f.Environment = append(f.Environment, "HTTPS_PROXY=http://different.invalid")
	if err := f.Validate(c.Binding()); !errors.Is(err, ErrChanged) {
		t.Fatal("proxy profile not bound", err)
	}
	if err := os.WriteFile(f.ConfigFile, []byte("[NASTOOL]\npassword_command = python helper.py\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if f.Available() {
		t.Fatal("password helper accepted")
	}
	for _, config := range []string{"RCLONE_ENCRYPT_V0:\nencrypted-data", "[NASTOOL]\ntype=sftp\nssh = python ssh.py\n"} {
		if err := os.WriteFile(f.ConfigFile, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if f.Available() {
			t.Fatal("uninspectable or external helper accepted")
		}
	}
}
func TestCommandUsesIsolatedConfigAndFixedFlagsWithoutShellOrInheritedRcloneFlags(t *testing.T) {
	f := testFactory(t, "args")
	f.Environment = append(f.Environment, "RCLONE_LOG_FILE=/secret-output", "RCLONE_IGNORE_EXISTING=true")
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out bytes.Buffer
	if err := c.command(t.Context(), []string{"literal;$(touch unexpected)"}, nil, &out, 4096); err != nil {
		t.Fatal(err)
	}
	var args []string
	if json.Unmarshal(out.Bytes(), &args) != nil {
		t.Fatal(out.String())
	}
	want := []string{"--config", c.config, "--retries", "1", "--low-level-retries", "1", "--stats", "0", "--log-level", "ERROR", "--buffer-size", "0", "--temp-dir", c.dir, "literal;$(touch unexpected)"}
	if digest(args) != digest(want) {
		t.Fatal(args)
	}
	for _, env := range c.environment {
		if strings.HasPrefix(env, "RCLONE_LOG_FILE=") || strings.HasPrefix(env, "RCLONE_IGNORE_EXISTING=") {
			t.Fatal("inherited unsafe flags", env)
		}
	}
}
func TestBoundedCommandsRedactCredentialsAndRespectCancellationAndOutputLimits(t *testing.T) {
	for _, mode := range []string{"stderr", "wait", "large"} {
		t.Run(mode, func(t *testing.T) {
			f := testFactory(t, mode)
			c, err := f.Open(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = c.command(ctx, []string{"lsjson", "NASTOOL:/library"}, nil, io.Discard, 1024)
			if err == nil || strings.Contains(err.Error(), "private-token") || time.Since(start) > 5*time.Second {
				t.Fatal(err)
			}
		})
	}
}
func TestInventoryRejectsTraversalLinksDuplicateNamesAndNestedFlatListings(t *testing.T) {
	for _, raw := range []string{`[{"Path":"../escape","Size":0}]`, `[{"Path":"file.rclonelink","Size":0}]`, `[{"Path":"A","Size":0},{"Path":"a","Size":0}]`, `[{"Path":"é","Size":0},{"Path":"e\u0301","Size":0}]`, `[{"Path":"sub/file","Size":0}]`, `[{"Path":"a","Size":-1}]`} {
		f := testFactory(t, "")
		f.Environment = append(f.Environment, "RCLONE_CONFIG_NASTOOL_TEST_JSON="+raw)
		c, err := f.Open(nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.List(t.Context(), "/library", false)
		c.Close()
		if err == nil {
			t.Fatal("unsafe inventory", raw)
		}
	}
}

func TestPreservingAnEquivalentLeafDoesNotGrantEquivalentAncestorTraversal(t *testing.T) {
	f := testFactory(t, "")
	f.Environment = append(f.Environment, `RCLONE_CONFIG_NASTOOL_TEST_JSON=[{"Path":"Movie.nfo","Size":12}]`)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Entry(t.Context(), "/library", "movie.nfo"); !errors.Is(err, ErrChanged) {
		t.Fatal("strict lookup adopted a case alias", err)
	}
	if exists, err := c.Existing(t.Context(), "/library", "movie.nfo"); err != nil || !exists {
		t.Fatal("safe preservation rejected", exists, err)
	}
	if _, err := c.Existing(t.Context(), "/library", "movie.nfo/child"); err == nil {
		t.Fatal("ancestor alias accepted")
	}
}
func TestRealRcloneTemporaryAliasRoundTripAndImmutableCopy(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("set an explicit Rclone test binary; never use operator accounts")
	}
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(cfg, []byte("[NASTOOL]\ntype = alias\nremote = "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "library"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := (Factory{ConfigFile: cfg, Binary: binary, Environment: []string{}}).Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Probe(t.Context(), "/library"); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(t.Context(), "/library/private/payload", 5, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(t.Context(), "/library/private/payload", "/library/media/file"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Read(t.Context(), "/library/media/file", 5, &out); err != nil || out.String() != "first" {
		t.Fatal(out.String(), err)
	}
	if err := c.Upload(t.Context(), "/library/private/payload", 6, strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(t.Context(), "/library/private/payload", "/library/media/file"); err == nil {
		t.Fatal("immutable overwrite accepted")
	}
	data, err := os.ReadFile(filepath.Join(root, "library/media/file"))
	if err != nil || string(data) != "first" {
		t.Fatal(string(data), err)
	}
	if n, err := c.Entry(t.Context(), "/library", "media/missing"); err != nil || n != nil {
		t.Fatal(n, err)
	}
	if err := os.Symlink("file", filepath.Join(root, "library/media/alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Entry(t.Context(), "/library", "media/file"); err == nil {
		t.Fatal("symlink inventory accepted")
	}
}
