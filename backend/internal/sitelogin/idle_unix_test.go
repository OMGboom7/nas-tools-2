//go:build linux || darwin

package sitelogin

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Repeated original service initialization must not exhaust process file
// descriptors before a login is requested. The small limit belongs solely to
// this test child; the parent and user's session limits are unchanged.
func TestLoginIdleStoresDoNotExhaustProcessFiles(t *testing.T) {
	const pathKey = "NASTOOL_TEST_LOGIN_IDLE_DATABASE"
	if path := os.Getenv(pathKey); path != "" {
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		if limit.Cur > 64 {
			limit.Cur = 64
		}
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		stores := []*Store{}
		defer func() {
			for _, s := range stores {
				s.Close()
			}
		}()
		for i := 0; i < 96; i++ {
			s, err := Open(path)
			if err != nil {
				t.Fatalf("original store initialization %d exhausted resources: %v", i, err)
			}
			stores = append(stores, s)
		}
		original := os.Getenv("NASTOOL_TEST_LOGIN_IDLE_JOB")
		for _, s := range stores {
			r, err := s.Get(t.Context(), original)
			if err != nil || r.ID != original || r.State != "queued" {
				t.Fatal("reopened original receipt changed", r.State, err)
			}
		}
		return
	}
	f := newFixture(t)
	f.create(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoginIdleStoresDoNotExhaustProcessFiles$", "-test.count=1")
	child.Env = append(os.Environ(), pathKey+"="+f.path, "NASTOOL_TEST_LOGIN_IDLE_JOB="+f.r.ID)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("bounded-file child failed: %v\n%s", err, output)
	}
}
