package searchcache

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
)

func TestPrivateResourcesOwnershipExpiryAndRetry(t *testing.T) {
	store := New()
	now := time.Now()
	store.now = func() time.Time { return now }
	seeders := int64(5)
	ids, err := store.Put("alice", []externalindexer.Resource{{Title: "Movie", DownloadURL: "https://tracker.local/download?secret=hidden", Seeders: &seeders}})
	if err != nil || len(ids) != 1 || !strings.HasPrefix(ids[0], Prefix) || strings.Contains(ids[0], "hidden") {
		t.Fatal(ids, err)
	}
	seeders = 99
	if _, _, err := store.Claim("bob", ids[0]); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	resource, done, err := store.Claim("alice", ids[0])
	if err != nil || done || *resource.Seeders != 5 {
		t.Fatal(resource, done, err)
	}
	*resource.Seeders = 77
	if _, _, err := store.Claim("alice", ids[0]); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	store.Finish("alice", ids[0], false)
	resource, done, err = store.Claim("alice", ids[0])
	if err != nil || done || *resource.Seeders != 5 {
		t.Fatal(resource, done, err)
	}
	store.Finish("alice", ids[0], true)
	if _, done, err := store.Claim("alice", ids[0]); err != nil || !done {
		t.Fatal(done, err)
	}
	now = now.Add(30 * time.Minute)
	if _, _, err := store.Claim("alice", ids[0]); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	if len(store.entries) != 0 || store.bytes != 0 {
		t.Fatal("expired resource retained")
	}
}

func TestConcurrentClaimsAndCapacity(t *testing.T) {
	store := New()
	ids, err := store.Put("owner", []externalindexer.Resource{{Title: "Movie"}})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); _, _, err := store.Claim("owner", ids[0]); results <- err }()
	}
	workers.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal(success)
	}
	before := len(store.entries)
	if _, err := store.Put("owner", []externalindexer.Resource{{Description: strings.Repeat("x", maxBytes)}}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if len(store.entries) != before {
		t.Fatal("partial batch inserted")
	}
	if _, err := store.Put("owner", make([]externalindexer.Resource, maxEntries)); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if _, err := store.Put("", nil); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}
