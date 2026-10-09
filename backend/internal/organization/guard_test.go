//go:build linux || darwin

package organization

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func guardFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	d, _ := copyFixture(t)
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, id
}

func TestJobGuardExcludesSeparateConnectionsAndKeepsStableLockFile(t *testing.T) {
	s, path, id := guardFixture(t)
	other, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	g, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	file := filepath.Join(s.guardRoot, guardDirectory, Digest(id)+".lock")
	before, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if peer, err := other.AcquireJob(t.Context(), id); !errors.Is(err, ErrBusy) {
		if peer != nil {
			peer.Close()
		}
		t.Fatal("active guard not excluded", err)
	}
	g.Close()
	g.Close()
	peer, err := other.AcquireJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	after, err := os.Lstat(file)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("guard release replaced/deleted lock inode", err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || job.State != "ready" || job.Items[0].State != "planned" {
		t.Fatal("guard changed execution ledger", job, err)
	}
}

func TestJobGuardAllowsDifferentJobsAndCanonicalizesDatabaseAlias(t *testing.T) {
	s, path, id := guardFixture(t)
	g, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	d := job.Definition
	d.Entries[0].Target = "another/Movie.mkv"
	otherID, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	otherGuard, err := s.AcquireJob(t.Context(), otherID)
	if err != nil {
		t.Fatal("unrelated job blocked", err)
	}
	otherGuard.Close()
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	viaAlias, err := OpenStore(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer viaAlias.Close()
	if peer, err := viaAlias.AcquireJob(t.Context(), id); !errors.Is(err, ErrBusy) {
		if peer != nil {
			peer.Close()
		}
		t.Fatal("database alias bypassed guard", err)
	}
}

func TestJobGuardRejectsUnsafeNamespaceWithoutFollowingOrChangingIt(t *testing.T) {
	for _, kind := range []string{"directory-link", "directory-permissions", "file-link", "file-permissions", "file-content", "file-hardlink", "root-replaced", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s, _, id := guardFixture(t)
			dir := filepath.Join(s.guardRoot, guardDirectory)
			name := filepath.Join(dir, Digest(id)+".lock")
			ctx := t.Context()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if kind == "root-replaced" {
				s.guardRootIdentity = "foreign"
			} else if kind == "directory-link" {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory-permissions":
					if err := os.Chmod(dir, 0755); err != nil {
						t.Fatal(err)
					}
				case "file-link":
					if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), name); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.WriteFile(name, nil, 0600); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "file-permissions":
						if err := os.Chmod(name, 0644); err != nil {
							t.Fatal(err)
						}
					case "file-content":
						if err := os.WriteFile(name, []byte("foreign"), 0600); err != nil {
							t.Fatal(err)
						}
					case "file-hardlink":
						if err := os.Link(name, filepath.Join(t.TempDir(), "alias")); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if guard, err := s.AcquireJob(ctx, id); err == nil {
				guard.Close()
				t.Fatal("unsafe namespace accepted", kind)
			}
			if kind == "cancelled" || kind == "root-replaced" {
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("guard created despite cancellation/changed root", err)
				}
			}
			if kind == "file-content" {
				assertMoveBytes(t, name, "foreign")
			}
		})
	}
}

func TestJobGuardSubprocess(t *testing.T) {
	path := os.Getenv("NASTOOL_JOB_GUARD_TEST_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.AcquireJob(t.Context(), os.Getenv("NASTOOL_JOB_GUARD_TEST_ID"))
	if os.Getenv("NASTOOL_JOB_GUARD_TEST_EXPECT") == "busy" {
		if !errors.Is(err, ErrBusy) {
			if g != nil {
				g.Close()
			}
			t.Fatal("separate process bypassed guard", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("NASTOOL_JOB_GUARD_TEST_EXPECT") == "hold" {
		fmt.Fprintln(os.Stdout, "guard-ready")
		var data [1]byte
		_, _ = os.Stdin.Read(data[:])
	}
	g.Close()
}

func TestJobGuardProcessTerminationReleasesOnlyMutualExclusion(t *testing.T) {
	s, path, id := guardFixture(t)
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobGuardSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "NASTOOL_JOB_GUARD_TEST_DB="+path, "NASTOOL_JOB_GUARD_TEST_ID="+id, "NASTOOL_JOB_GUARD_TEST_EXPECT=hold")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "guard-ready\n" {
		t.Fatal("helper failed to acquire guard", line, err)
	}
	if peer, err := s.AcquireJob(t.Context(), id); !errors.Is(err, ErrBusy) {
		if peer != nil {
			peer.Close()
		}
		t.Fatal("live child not excluded", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("terminated helper reported success")
	}
	g, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		t.Fatal("kernel did not release dead actor guard", err)
	}
	g.Close()
	job, err := s.Get(t.Context(), id)
	if err != nil || job.Items[0].State != "running" || job.State != "needs_review" {
		t.Fatal("process death reset uncertain state", job, err)
	}
}

func TestJobGuardReallyExcludesOtherProcessesAndReopensAfterRelease(t *testing.T) {
	s, path, id := guardFixture(t)
	g, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	run := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobGuardSubprocess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "NASTOOL_JOB_GUARD_TEST_DB="+path, "NASTOOL_JOB_GUARD_TEST_ID="+id, "NASTOOL_JOB_GUARD_TEST_EXPECT="+want)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	run("busy")
	g.Close()
	run("free")
}
