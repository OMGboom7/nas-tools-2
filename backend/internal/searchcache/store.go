package searchcache

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
)

const Prefix = "go-search-"
const maxEntries = 2000
const maxBytes = 16 << 20

var ErrMissing = errors.New("search resource is missing or expired")
var ErrBusy = errors.New("search resource download is in progress")
var ErrCapacity = errors.New("search resource cache is full")

type entry struct {
	owner      string
	resource   externalindexer.Resource
	expires    time.Time
	cost       int
	busy, done bool
}

// The cache is intentionally ephemeral: restart/expiry requires searching
// again. Secret download URLs are never used as IDs or persisted on disk.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	bytes   int
	now     func() time.Time
}

func New() *Store { return &Store{entries: map[string]*entry{}, now: time.Now} }

func clone(resource externalindexer.Resource) externalindexer.Resource {
	if resource.Seeders != nil {
		v := *resource.Seeders
		resource.Seeders = &v
	}
	if resource.Peers != nil {
		v := *resource.Peers
		resource.Peers = &v
	}
	if resource.DownloadFactor != nil {
		v := *resource.DownloadFactor
		resource.DownloadFactor = &v
	}
	if resource.UploadFactor != nil {
		v := *resource.UploadFactor
		resource.UploadFactor = &v
	}
	if resource.Freeleech != nil {
		v := *resource.Freeleech
		resource.Freeleech = &v
	}
	if resource.MinimumSeedTime != nil {
		v := *resource.MinimumSeedTime
		resource.MinimumSeedTime = &v
	}
	if resource.MinimumRatio != nil {
		v := *resource.MinimumRatio
		resource.MinimumRatio = &v
	}
	return resource
}

func (store *Store) prune(now time.Time) {
	for id, item := range store.entries {
		if !item.busy && !now.Before(item.expires) {
			delete(store.entries, id)
			store.bytes -= item.cost
		}
	}
}

func (store *Store) Put(owner string, resources []externalindexer.Resource) ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.prune(now)
	if owner == "" || len(owner) > 256 || len(resources) > maxEntries || len(store.entries)+len(resources) > maxEntries {
		return nil, ErrCapacity
	}
	costs := make([]int, len(resources))
	total := 0
	for index, item := range resources {
		costs[index] = len(owner) + len(item.IndexerID) + len(item.Indexer) + len(item.Title) + len(item.DownloadURL) + len(item.DownloadResolver) + len(item.Description) + len(item.PageURL) + len(item.IMDbID) + 256
		total += costs[index]
		if total > maxBytes-store.bytes {
			return nil, ErrCapacity
		}
	}
	ids := make([]string, len(resources))
	pending := map[string]bool{}
	for index := range resources {
		var entropy [16]byte
		for {
			if _, err := rand.Read(entropy[:]); err != nil {
				return nil, err
			}
			id := Prefix + hex.EncodeToString(entropy[:])
			if store.entries[id] == nil && !pending[id] {
				ids[index] = id
				pending[id] = true
				break
			}
		}
	}
	for index, id := range ids {
		store.entries[id] = &entry{owner: owner, resource: clone(resources[index]), cost: costs[index], expires: now.Add(30 * time.Minute)}
	}
	store.bytes += total
	return ids, nil
}

// Claim serializes download attempts and binds them to the authenticated user.
// A completed download returns done=true without issuing another external call.
func (store *Store) Claim(owner, id string) (resource externalindexer.Resource, done bool, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.prune(now)
	item := store.entries[id]
	if !strings.HasPrefix(id, Prefix) || item == nil || item.owner != owner || !now.Before(item.expires) {
		return resource, false, ErrMissing
	}
	if item.done {
		return clone(item.resource), true, nil
	}
	if item.busy {
		return resource, false, ErrBusy
	}
	item.busy = true
	return clone(item.resource), false, nil
}

// Finish leaves failed attempts retryable. A downloader transport failure can
// be ambiguous; clients must check the downloader before retrying that case.
func (store *Store) Finish(owner, id string, success bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if item := store.entries[id]; item != nil && item.owner == owner {
		item.busy = false
		item.done = success
	}
}
