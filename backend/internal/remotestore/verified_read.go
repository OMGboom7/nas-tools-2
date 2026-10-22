package remotestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
)

// These limits bound an in-memory hash index and a single decoder session, not
// the normal transfer protocol. No second video or cloud credentials are saved.
const VerifiedBlockSize = 4 << 20
const MaxVerifiedMediaSize int64 = 1 << 40 // at most 8 MiB of block hashes
const maxVerifiedReadBytes int64 = 512 << 20
const maxVerifiedReads = 256

type byteCounter struct {
	out io.Writer
	n   int64
}

func (w *byteCounter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.n += int64(n)
	return n, err
}

// ReadRange reads exactly one bounded range. It is not permission to adopt an
// arbitrary object: VerifiedObject additionally binds complete and block hashes.
func (c *Client) ReadRange(ctx context.Context, location string, size, offset, count int64, out io.Writer) error {
	if out == nil || size < 1 || size > 1<<50 || offset < 0 || count < 1 || count > VerifiedBlockSize || offset >= size || count > size-offset {
		return ErrLimit
	}
	if c.s3 != nil {
		return c.minioReadRange(ctx, location, size, offset, count, out)
	}
	a, err := address(location)
	if err != nil {
		return err
	}
	w := &byteCounter{out: out}
	if err := c.command(ctx, []string{"cat", a, "--offset", strconv.FormatInt(offset, 10), "--count", strconv.FormatInt(count, 10)}, nil, w, count); err != nil {
		return err
	}
	if w.n != count {
		return ErrChanged
	}
	return nil
}

type blockIndex struct {
	whole, block hash.Hash
	blocks       [][sha256.Size]byte
	size, count  int64
	used         int
}

func (w *blockIndex) Write(p []byte) (int, error) {
	if int64(len(p)) > w.size-w.count {
		return 0, ErrChanged
	}
	length := len(p)
	w.whole.Write(p)
	w.count += int64(length)
	for len(p) > 0 {
		n := min(len(p), VerifiedBlockSize-w.used)
		w.block.Write(p[:n])
		w.used += n
		p = p[n:]
		if w.used == VerifiedBlockSize {
			w.finish()
		}
	}
	return length, nil
}

func (w *blockIndex) finish() {
	if w.used == 0 {
		return
	}
	w.blocks = append(w.blocks, [sha256.Size]byte(w.block.Sum(nil)))
	w.block.Reset()
	w.used = 0
}

// VerifiedObject is a short-lived readonly capability for exactly one proven
// object. Each fetched block must match the index derived from its full SHA-256
// before any of that block can reach a decoder. Metadata/ETags alone do not
// establish content identity, including a same-size/same-time replacement.
type VerifiedObject struct {
	client         *Client
	root, relative string
	node           Node
	blocks         [][sha256.Size]byte
	ctx            context.Context
	cancel         context.CancelFunc
	current        func(context.Context) error
	closed, failed atomic.Bool
	mu             sync.Mutex
	readBytes      int64
	reads          int
}

func (c *Client) Pin(ctx context.Context, root, relativePath string, size int64, want string, current func(context.Context) error) (*VerifiedObject, error) {
	digest, err := hex.DecodeString(want)
	if err != nil || len(digest) != sha256.Size || size < 1 || size > MaxVerifiedMediaSize || current == nil || !safeRoot(root) || !relative(relativePath) {
		return nil, ErrLimit
	}
	call, cancel := context.WithCancel(ctx)
	o := &VerifiedObject{client: c, root: root, relative: relativePath, ctx: call, cancel: cancel, current: current}
	ok := false
	defer func() {
		if !ok {
			o.Close()
		}
	}()
	if err := current(call); err != nil {
		return nil, err
	}
	node, err := c.Entry(call, root, relativePath)
	if err != nil || node == nil || node.IsDir || node.Size != size {
		return nil, ErrChanged
	}
	o.node = *node
	index := &blockIndex{whole: sha256.New(), block: sha256.New(), size: size, blocks: make([][sha256.Size]byte, 0, (size+VerifiedBlockSize-1)/VerifiedBlockSize)}
	if err := c.Read(call, path.Join(root, relativePath), size, index); err != nil {
		return nil, err
	}
	index.finish()
	if index.count != size || !bytes.Equal(index.whole.Sum(nil), digest) {
		return nil, ErrChanged
	}
	o.blocks = index.blocks
	if err := o.Current(call); err != nil {
		return nil, err
	}
	ok = true
	return o, nil
}

func (o *VerifiedObject) Size() int64 { return o.node.Size }
func (o *VerifiedObject) Close() {
	if !o.closed.Swap(true) {
		o.cancel()
	}
}

func (o *VerifiedObject) Current(ctx context.Context) error {
	if o.closed.Load() || o.failed.Load() || o.ctx.Err() != nil || ctx.Err() != nil {
		return ErrChanged
	}
	if err := o.current(ctx); err != nil {
		return err
	}
	n, err := o.client.Entry(ctx, o.root, o.relative)
	if err != nil || n == nil || *n != o.node {
		return ErrChanged
	}
	return o.client.Current()
}

// ReadAt has ReaderAt's EOF semantics and a context for each request. It never
// exposes a partial unverified block, retains no whole-video cache and cannot
// outlive its enclosing job/capability, even with a caller's fresh context.
func (o *VerifiedObject) ReadAt(ctx context.Context, p []byte, offset int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	defer stop()
	defer cancel()
	if offset < 0 || len(p) > VerifiedBlockSize {
		return 0, ErrLimit
	}
	if err := o.Current(call); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if offset >= o.Size() {
		return 0, io.EOF
	}
	length := min(int64(len(p)), o.Size()-offset)
	data := make([]byte, 0, length)
	for at := offset; int64(len(data)) < length; {
		block := at / VerifiedBlockSize
		start := block * VerifiedBlockSize
		count := min(int64(VerifiedBlockSize), o.Size()-start)
		if o.reads >= maxVerifiedReads || count > maxVerifiedReadBytes-o.readBytes {
			o.failed.Store(true)
			return 0, ErrLimit
		}
		o.readBytes += count
		o.reads++
		var b bytes.Buffer
		b.Grow(int(count))
		if err := o.client.ReadRange(call, path.Join(o.root, o.relative), o.Size(), start, count, &b); err != nil {
			if call.Err() == nil {
				o.failed.Store(true)
			}
			return 0, err
		}
		if sha256.Sum256(b.Bytes()) != o.blocks[block] {
			o.failed.Store(true)
			return 0, ErrChanged
		}
		n := min(length-int64(len(data)), count-(at-start))
		data = append(data, b.Bytes()[at-start:at-start+n]...)
		at += n
	}
	if err := o.Current(call); err != nil {
		return 0, err
	}
	copy(p, data)
	if length < int64(len(p)) {
		return int(length), io.EOF
	}
	return len(p), nil
}
