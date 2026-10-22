//go:build linux || darwin

package organization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func LibraryTarget(d LibraryDefinition, m LibraryMedia, scope, name string) (string, error) {
	kind := "电影"
	if d.Root.Kind != "movie" {
		kind = "电视剧"
	}
	entry := Entry{Target: m.Path, Kind: "media", MediaType: kind}
	if m.Disc != nil {
		entry.Disc = &DiscInfo{}
		if m.Path == "." {
			// A configured root can itself be a disc. Its movie metadata stays
			// inside that root; never invent a writable show parent outside it.
			if scope != "media" {
				return "", ErrPath
			}
			entry.Target = LibraryDiscStem(d, m)
			target, err := sidecarTarget(JobItem{Entry: entry}, scope, name)
			if err != nil {
				return "", err
			}
			return filepath.Base(target), nil
		}
	}
	return sidecarTarget(JobItem{Entry: entry}, scope, name)
}
func libraryHash(ctx context.Context, parent *os.File, name string) (LibraryNode, string, error) {
	before, kind, err := libraryNode(parent, name)
	if err != nil {
		return before, "", err
	}
	if kind != unix.S_IFREG || before.Size < 0 || before.Size > 16<<20 {
		return before, "", ErrPath
	}
	f, err := openRegular(parent, name)
	if err != nil {
		return before, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != before.Identity {
		return before, "", ErrState
	}
	hash, err := hashSource(ctx, f, before.Size)
	if err != nil {
		return before, "", err
	}
	after, _, err := libraryNode(parent, name)
	if err != nil || before != after {
		return before, "", ErrState
	}
	return before, hash, nil
}

func libraryLinkHash(parent *os.File, name, expected string) (LibraryNode, string, error) {
	before, kind, err := libraryNode(parent, name)
	if err != nil || kind != unix.S_IFLNK {
		return before, "", ErrState
	}
	link, err := readLinkAt(parent, name)
	if err != nil || link != expected {
		return before, "", ErrState
	}
	after, _, err := libraryNode(parent, name)
	if err != nil || before != after {
		return before, "", ErrState
	}
	sum := sha256.Sum256([]byte(link))
	return before, hex.EncodeToString(sum[:]), nil
}
func libraryOldHash(ctx context.Context, parent *os.File, name string, p LibraryOutputProof) (LibraryNode, string, error) {
	if p.OldLink != "" {
		return libraryLinkHash(parent, name, p.OldLink)
	}
	return libraryHash(ctx, parent, name)
}

// Recovery can observe the new regular file or the original symlink. Never
// follow that link; the exact inode and old/new digest must still match.
func libraryRecoveryHash(ctx context.Context, parent *os.File, name string, p LibraryOutputProof) (LibraryNode, string, error) {
	node, hash, err := libraryHash(ctx, parent, name)
	if err != nil && p.OldLink != "" {
		return libraryLinkHash(parent, name, p.OldLink)
	}
	return node, hash, err
}
func libraryOpenStage(parent *os.File, p LibraryOutputProof) (*os.File, error) {
	if !strings.HasPrefix(p.Stage, ".nastool-library-") || filepath.Base(p.Stage) != p.Stage {
		return nil, ErrPath
	}
	fd, err := unix.Openat(int(parent.Fd()), p.Stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), p.Stage)
	info, err := f.Stat()
	var st unix.Stat_t
	if err != nil || Identity(info) != p.StageIdentity || info.Mode().Perm() != 0700 || unix.Fstat(fd, &st) != nil || int(st.Uid) != os.Geteuid() {
		f.Close()
		return nil, ErrPath
	}
	return f, nil
}
func LibraryExisting(ctx context.Context, d LibraryDefinition, m LibraryMedia, scope, name string) (bool, error) {
	if d.Remote != nil {
		if VerifyLibraryMedia(ctx, d, m) != nil {
			return false, ErrState
		}
		target, err := LibraryTarget(d, m, scope, name)
		if err != nil {
			return false, err
		}
		c, err := libraryRemoteClient(ctx, d)
		if err != nil {
			return false, err
		}
		return c.Existing(ctx, d.Root.Path, target)
	}
	if VerifyLibraryMedia(ctx, d, m) != nil {
		return false, ErrState
	}
	target, err := LibraryTarget(d, m, scope, name)
	if err != nil {
		return false, err
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return false, err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, target, false)
	if err != nil {
		return false, err
	}
	defer p.Close()
	_, _, err = libraryNode(p, n)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}

// Force mode exchanges objects, never truncates one. The object actually
// displaced by the exchange remains in the private stage even if a late race
// makes it differ from the expected old node; that outcome requires review.
func PublishLibrarySidecar(ctx context.Context, d LibraryDefinition, m LibraryMedia, scope, name string, data []byte, prepare func(LibraryOutputProof) error, current func() error) (proof LibraryOutputProof, state string, err error) {
	if d.Remote != nil {
		return publishLibraryRemoteSidecar(ctx, d, m, scope, name, data, prepare, current)
	}
	state = "blocked"
	if !LibraryMode(d.Mode) || len(data) == 0 || len(data) > 16<<20 || prepare == nil || current == nil || VerifyLibraryMedia(ctx, d, m) != nil {
		return proof, state, ErrState
	}
	proof.Target, err = LibraryTarget(d, m, scope, name)
	if err != nil {
		return proof, state, err
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return proof, state, err
	}
	defer r.Close()
	parent, leaf, err := anchoredParent(r, proof.Target, false)
	if err != nil {
		return proof, state, err
	}
	defer parent.Close()
	pi, err := parent.Stat()
	if err != nil {
		return proof, state, err
	}
	proof.ParentIdentity = Identity(pi)
	overwrite := LibraryOverwrite(d.Mode, name)
	if !overwrite && d.Root.Kind == "movie" && strings.HasSuffix(name, ".nfo") {
		if _, _, e := libraryNode(parent, "movie.nfo"); e == nil {
			return proof, "preserved", nil
		} else if !errors.Is(e, unix.ENOENT) {
			return proof, state, e
		}
	}
	if _, kind, e := libraryNode(parent, leaf); e == nil {
		if !overwrite {
			return proof, "preserved", nil
		}
		if kind == unix.S_IFLNK && m.Disc != nil {
			proof.OldLink, err = readLinkAt(parent, leaf)
			if err != nil || proof.OldLink == "" {
				return proof, state, ErrState
			}
		}
		proof.Old, proof.OldDigest, err = libraryOldHash(ctx, parent, leaf, proof)
		if err != nil {
			return proof, state, err
		}
	} else if !errors.Is(e, unix.ENOENT) {
		return proof, state, e
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return proof, state, err
	}
	proof.Stage = ".nastool-library-" + hex.EncodeToString(nonce[:])
	if err := unix.Mkdirat(int(parent.Fd()), proof.Stage, 0700); err != nil {
		return proof, state, err
	}
	fd, err := unix.Openat(int(parent.Fd()), proof.Stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return proof, state, err
	}
	stage := os.NewFile(uintptr(fd), proof.Stage)
	defer stage.Close()
	si, err := stage.Stat()
	if err != nil {
		return proof, state, err
	}
	proof.StageIdentity = Identity(si)
	owned, err := libraryOpenStage(parent, proof)
	if err != nil {
		return proof, state, err
	}
	owned.Close()
	payloadFD, err := unix.Openat(fd, "payload", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return proof, state, err
	}
	f := os.NewFile(uintptr(payloadFD), "library metadata payload")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return proof, state, err
	}
	proof.NewIdentity, proof.NewSize = Identity(info), int64(len(data))
	sum := sha256.Sum256(data)
	proof.NewDigest = hex.EncodeToString(sum[:])
	keep := false
	defer func() {
		if keep {
			return
		}
		node, kind, e := libraryNode(stage, "payload")
		if e == nil && kind == unix.S_IFREG && node.Identity == proof.NewIdentity {
			_ = unix.Unlinkat(fd, "payload", 0)
		}
		actual, e := libraryOpenStage(parent, proof)
		if e == nil {
			actual.Close()
			_ = unix.Unlinkat(int(parent.Fd()), proof.Stage, unix.AT_REMOVEDIR)
		}
	}()
	if err := prepare(proof); err != nil {
		return proof, state, err
	}
	if ctx.Err() != nil {
		return proof, state, ctx.Err()
	}
	if _, err := f.Write(data); err != nil {
		return proof, state, err
	}
	if err := f.Chmod(0644); err != nil {
		return proof, state, err
	}
	if err := f.Sync(); err != nil {
		return proof, state, err
	}
	check := func() error {
		if VerifyLibraryMedia(ctx, d, m) != nil {
			return ErrState
		}
		root, e := anchoredRoot(d.Root.Path, d.Root.Identity)
		if e != nil {
			return e
		}
		defer root.Close()
		p, _, e := anchoredParent(root, proof.Target, false)
		if e != nil {
			return e
		}
		defer p.Close()
		info, e := p.Stat()
		if e != nil || Identity(info) != proof.ParentIdentity {
			return ErrPath
		}
		return current()
	}
	if err := check(); err != nil {
		return proof, state, err
	}
	actual, err := libraryOpenStage(parent, proof)
	if err != nil {
		return proof, state, err
	}
	actual.Close()
	node, hash, err := libraryHash(ctx, stage, "payload")
	if err != nil || node.Identity != proof.NewIdentity || node.Links != 1 || hash != proof.NewDigest {
		return proof, state, ErrState
	}
	if proof.Old.Identity != "" {
		node, hash, err := libraryOldHash(ctx, parent, leaf, proof)
		if err != nil || node != proof.Old || hash != proof.OldDigest {
			return proof, state, ErrState
		}
		if err := check(); err != nil {
			return proof, state, err
		}
		err = swapFiles(fd, "payload", int(parent.Fd()), leaf)
		if err != nil {
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV) {
				return proof, state, ErrMode
			}
			keep = true
			return proof, "uncertain", err
		}
		keep = true
	} else {
		if !overwrite && d.Root.Kind == "movie" && strings.HasSuffix(name, ".nfo") {
			if _, _, e := libraryNode(parent, "movie.nfo"); e == nil {
				return proof, "preserved", nil
			} else if !errors.Is(e, unix.ENOENT) {
				return proof, state, e
			}
		}
		if err := renameNoReplace(fd, "payload", int(parent.Fd()), leaf); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return proof, "preserved", nil
			}
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV) {
				return proof, state, ErrMode
			}
			keep = true
			return proof, "uncertain", err
		}
	}
	state = "uncertain"
	if err := stage.Sync(); err != nil {
		return proof, state, err
	}
	if err := parent.Sync(); err != nil {
		return proof, state, err
	}
	if err := check(); err != nil {
		return proof, state, err
	}
	node, hash, err = libraryHash(ctx, parent, leaf)
	if err != nil || node.Identity != proof.NewIdentity || hash != proof.NewDigest {
		return proof, state, ErrState
	}
	if proof.Old.Identity != "" {
		old, hash, err := libraryOldHash(ctx, stage, "payload", proof)
		if err != nil || old.Identity != proof.Old.Identity || hash != proof.OldDigest {
			return proof, state, ErrState
		}
		return proof, "replaced", nil
	}
	return proof, "created", nil
}

func RestoreLibrarySidecar(ctx context.Context, d LibraryDefinition, p LibraryOutputProof, intent func() error) (string, error) {
	if d.Remote != nil {
		return restoreLibraryRemoteSidecar(ctx, d, p, intent)
	}
	if !validRelative(p.Target) || p.Old.Identity == "" || len(p.OldDigest) != 64 || len(p.NewDigest) != 64 || intent == nil {
		return "uncertain", ErrState
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return "uncertain", err
	}
	defer r.Close()
	parent, leaf, err := anchoredParent(r, p.Target, false)
	if err != nil {
		return "uncertain", err
	}
	defer parent.Close()
	pi, err := parent.Stat()
	if err != nil || Identity(pi) != p.ParentIdentity {
		return "uncertain", ErrPath
	}
	stage, err := libraryOpenStage(parent, p)
	if err != nil {
		return "uncertain", err
	}
	defer stage.Close()
	current, hash, err := libraryRecoveryHash(ctx, parent, leaf, p)
	if err != nil {
		return "uncertain", err
	}
	backup, backupHash, err := libraryRecoveryHash(ctx, stage, "payload", p)
	if err != nil {
		return "uncertain", err
	}
	if current.Identity == p.Old.Identity && hash == p.OldDigest && backup.Identity == p.NewIdentity && backupHash == p.NewDigest {
		if err := libraryProofCurrent(d, p); err != nil {
			return "uncertain", err
		}
		if err := intent(); err != nil {
			return "uncertain", err
		}
		return "restored", nil
	}
	if current.Identity != p.NewIdentity || hash != p.NewDigest || backup.Identity != p.Old.Identity || backupHash != p.OldDigest {
		return "uncertain", ErrState
	}
	if err := intent(); err != nil {
		return "uncertain", err
	}
	if ctx.Err() != nil {
		return "uncertain", ctx.Err()
	}
	if err := libraryProofCurrent(d, p); err != nil {
		return "uncertain", err
	}
	current, hash, err = libraryHash(ctx, parent, leaf)
	backup, backupHash, backupErr := libraryOldHash(ctx, stage, "payload", p)
	if err != nil || backupErr != nil || current.Identity != p.NewIdentity || hash != p.NewDigest || backup.Identity != p.Old.Identity || backupHash != p.OldDigest {
		return "uncertain", ErrState
	}
	if err := swapFiles(int(stage.Fd()), "payload", int(parent.Fd()), leaf); err != nil {
		return "uncertain", err
	}
	if err := stage.Sync(); err != nil {
		return "uncertain", err
	}
	if err := parent.Sync(); err != nil {
		return "uncertain", err
	}
	actual, hash, err := libraryOldHash(ctx, parent, leaf, p)
	if err != nil || actual.Identity != p.Old.Identity || hash != p.OldDigest {
		return "uncertain", ErrState
	}
	newNode, hash, err := libraryHash(ctx, stage, "payload")
	if err != nil || newNode.Identity != p.NewIdentity || hash != p.NewDigest {
		return "uncertain", ErrState
	}
	if err := libraryProofCurrent(d, p); err != nil {
		return "uncertain", err
	}
	return "restored", nil
}

func libraryProofCurrent(d LibraryDefinition, p LibraryOutputProof) error {
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return err
	}
	defer r.Close()
	parent, _, err := anchoredParent(r, p.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	if p.Old.Identity != "" {
		stage, err := libraryOpenStage(parent, p)
		if err != nil {
			return err
		}
		stage.Close()
	}
	return nil
}

// ObserveLibrarySidecar establishes a positive current outcome, not a reason
// to retry an interrupted write. Incomplete stages are retained untouched.
func ObserveLibrarySidecar(ctx context.Context, d LibraryDefinition, p LibraryOutputProof) (string, error) {
	if d.Remote != nil {
		return observeLibraryRemoteSidecar(ctx, d, p)
	}
	if !validRelative(p.Target) || p.Target == "." || p.NewIdentity == "" || len(p.NewDigest) != 64 {
		return "uncertain", ErrState
	}
	if err := libraryProofCurrent(d, p); err != nil {
		return "uncertain", err
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return "uncertain", err
	}
	defer r.Close()
	parent, leaf, err := anchoredParent(r, p.Target, false)
	if err != nil {
		return "uncertain", err
	}
	defer parent.Close()
	current, hash, e := libraryHash(ctx, parent, leaf)
	if e == nil && current.Identity == p.NewIdentity && hash == p.NewDigest && current.Links == 1 {
		if p.Old.Identity == "" {
			if err := libraryProofCurrent(d, p); err != nil {
				return "uncertain", err
			}
			return "created", nil
		}
		stage, err := libraryOpenStage(parent, p)
		if err != nil {
			return "uncertain", err
		}
		defer stage.Close()
		old, oldHash, err := libraryOldHash(ctx, stage, "payload", p)
		if err == nil && old.Identity == p.Old.Identity && oldHash == p.OldDigest {
			if err := libraryProofCurrent(d, p); err != nil {
				return "uncertain", err
			}
			return "replaced", nil
		}
		return "uncertain", ErrState
	}
	old, oldHash, oldErr := libraryOldHash(ctx, parent, leaf, p)
	unchanged := p.Old.Identity != "" && oldErr == nil && old == p.Old && oldHash == p.OldDigest || p.Old.Identity == "" && errors.Is(e, unix.ENOENT)
	if !unchanged {
		return "uncertain", ErrState
	}
	stage, err := libraryOpenStage(parent, p)
	if err != nil {
		return "uncertain", err
	}
	defer stage.Close()
	node, kind, err := libraryNode(stage, "payload")
	if err != nil || kind != unix.S_IFREG || node.Identity != p.NewIdentity || node.Links != 1 || node.Size > p.NewSize {
		return "uncertain", ErrState
	}
	if err := libraryProofCurrent(d, p); err != nil {
		return "uncertain", err
	}
	return "not_published", nil
}
