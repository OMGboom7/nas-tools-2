//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Each directory is opened relative to a pinned descriptor, without following
// links. The leaf name is never combined into an unrestricted write path.
func anchoredRoot(path, identity string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return nil, ErrPath
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, ErrPath
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || Identity(info) != identity {
		f.Close()
		return nil, ErrPath
	}
	return f, nil
}

func anchoredParent(root *os.File, relative string, create bool) (*os.File, string, error) {
	if !validRelative(relative) || relative == "." {
		return nil, "", ErrPath
	}
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, "", err
	}
	unix.CloseOnExec(fd)
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(fd, part, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return nil, "", err
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, "", ErrPath
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Dir(relative)), parts[len(parts)-1], nil
}

func openRegular(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrPath
	}
	return f, nil
}

func matches(info os.FileInfo, item Entry) bool {
	return info.Mode().IsRegular() && Identity(info) == item.Identity && info.Size() == item.Size && strconv.FormatInt(info.ModTime().UnixNano(), 10) == item.Modified
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func Copy(ctx context.Context, d Definition, item Entry, tempName string, prepared func(Proof) error) (Proof, error) {
	if d.Mode != "copy" {
		return Proof{}, ErrMode
	}
	return Transfer(ctx, d, item, tempName, prepared)
}

// Transfer never removes the source or overwrites a target. The journal mode
// is executed literally: unsupported links never silently turn into copies.
func Transfer(ctx context.Context, d Definition, item Entry, tempName string, prepared func(Proof) error) (Proof, error) {
	var proof Proof
	if !SupportedMode(d.Mode) || d.Mode == "move" {
		return proof, ErrMode
	}
	if !strings.HasPrefix(tempName, ".nastool-copy-") || filepath.Base(tempName) != tempName || !validRelative(tempName) {
		return proof, ErrPath
	}
	if err := ctx.Err(); err != nil {
		return proof, err
	}
	if err := ValidateMode(ctx, d, item); err != nil {
		return proof, err
	}
	sourceRoot, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return proof, err
	}
	defer sourceRoot.Close()
	targetRoot, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return proof, err
	}
	defer targetRoot.Close()
	sourceParent, sourceName, err := anchoredParent(sourceRoot, item.Source, false)
	if err != nil {
		return proof, err
	}
	defer sourceParent.Close()
	source, err := openRegular(sourceParent, sourceName)
	if err != nil {
		return proof, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !matches(info, item) {
		return proof, ErrPath
	}
	// A stable snapshot is not a downloader completion signal. Manual execution
	// also requires a quiet interval; automatic completion remains a later module.
	if time.Since(info.ModTime()) < 30*time.Second {
		return proof, ErrState
	}
	targetParent, targetName, err := anchoredParent(targetRoot, item.Target, true)
	if err != nil {
		return proof, err
	}
	defer targetParent.Close()
	if d.Mode == "link" {
		parentInfo, err := targetParent.Stat()
		if err != nil {
			return proof, err
		}
		if !sameDevice(Identity(info), Identity(parentInfo)) {
			return proof, ErrMode
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(targetParent.Fd()), targetName, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil || !errors.Is(err, unix.ENOENT) {
		return proof, ErrClaimed
	}
	// A private directory prevents other directory writers from substituting
	// the staging leaf between proof persistence and atomic publication.
	if err = unix.Mkdirat(int(targetParent.Fd()), tempName, 0700); err != nil {
		return proof, ErrState
	}
	stageFD, err := unix.Openat(int(targetParent.Fd()), tempName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return proof, err
	}
	stage := os.NewFile(uintptr(stageFD), tempName)
	defer stage.Close()
	var stageStat unix.Stat_t
	if err = unix.Fstat(stageFD, &stageStat); err != nil || stageStat.Uid != uint32(os.Geteuid()) || stageStat.Mode&0777 != 0700 {
		return proof, ErrPath
	}
	proof, err = preparePayload(ctx, d, item, source, info, sourceParent, sourceName, stage)
	if err != nil {
		return proof, err
	}
	info, err = source.Stat()
	if err != nil || !matches(info, item) {
		return proof, ErrPath
	}
	current, err := openRegular(sourceParent, sourceName)
	if err != nil {
		return proof, ErrPath
	}
	currentInfo, err := current.Stat()
	current.Close()
	if err != nil || !matches(currentInfo, item) {
		return proof, ErrPath
	}
	parentInfo, err := targetParent.Stat()
	if err != nil {
		return proof, err
	}
	stageInfo, err := stage.Stat()
	if err != nil {
		return proof, err
	}
	proof.Temp, proof.ParentIdentity, proof.StagingIdentity = tempName, Identity(parentInfo), Identity(stageInfo)
	// Persist the inode + digest and flush directory entries BEFORE publication.
	if err = stage.Sync(); err != nil {
		return proof, err
	}
	if err = targetParent.Sync(); err != nil {
		return proof, err
	}
	if err = prepared(proof); err != nil {
		return proof, err
	}
	if err = ctx.Err(); err != nil {
		return proof, err
	}
	info, err = source.Stat()
	if err != nil || !matches(info, item) {
		return proof, ErrPath
	}
	current, err = openRegular(sourceParent, sourceName)
	if err != nil {
		return proof, ErrPath
	}
	currentInfo, err = current.Stat()
	current.Close()
	if err != nil || !matches(currentInfo, item) {
		return proof, ErrPath
	}
	if err := validateSourceBinding(d, item); err != nil {
		return proof, err
	}
	// Reject a root or target directory replaced while the copy was prepared.
	checkRoot, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return proof, err
	}
	checkParent, _, err := anchoredParent(checkRoot, item.Target, false)
	checkRoot.Close()
	if err != nil {
		return proof, err
	}
	checkInfo, err := checkParent.Stat()
	checkParent.Close()
	if err != nil || Identity(checkInfo) != proof.ParentIdentity {
		return proof, ErrPath
	}
	// Keep the private staging inode alive until journal completion, so even
	// inode reuse after a target deletion cannot masquerade as our publication.
	if err = unix.Linkat(stageFD, "payload", int(targetParent.Fd()), targetName, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return proof, ErrClaimed
		}
		return proof, modeOperationError(err)
	}
	if err = stage.Sync(); err != nil {
		return proof, err
	}
	if err = targetParent.Sync(); err != nil {
		return proof, err
	}
	return proof, nil
}

// VerifyPublished proves ownership, not mere existence/equal size. An unrelated
// target or a partial staging file can never be reconciled into successful history.
func VerifyPublished(ctx context.Context, d Definition, item Entry, p Proof) error {
	if p.Identity == "" || len(p.Digest) != 64 || p.ParentIdentity == "" || p.StagingIdentity == "" || !strings.HasPrefix(p.Temp, ".nastool-copy-") || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) {
		return ErrState
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, name, err := anchoredParent(root, item.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil || Identity(parentInfo) != p.ParentIdentity {
		return ErrPath
	}
	stageFD, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrState
	}
	stage := os.NewFile(uintptr(stageFD), p.Temp)
	defer stage.Close()
	stageInfo, err := stage.Stat()
	if err != nil || Identity(stageInfo) != p.StagingIdentity {
		return ErrState
	}
	if d.Mode == "softlink" {
		return verifySoftlink(ctx, d, item, p, parent, name, stage)
	}
	if d.Mode != "copy" && d.Mode != "link" || p.Kind != "" && p.Kind != "regular" || p.LinkTarget != "" {
		return ErrState
	}
	if d.Mode == "link" && p.Identity != item.Identity {
		return ErrState
	}
	anchor, err := openRegular(stage, "payload")
	if err != nil {
		return ErrState
	}
	defer anchor.Close()
	anchorInfo, err := anchor.Stat()
	if err != nil || Identity(anchorInfo) != p.Identity {
		return ErrState
	}
	target, err := openRegular(parent, name)
	if err != nil {
		return err
	}
	defer target.Close()
	info, err := target.Stat()
	if err != nil || Identity(info) != p.Identity || info.Size() != item.Size {
		return ErrState
	}
	if d.Mode == "link" && !matches(info, item) {
		return ErrPath
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, contextReader{ctx, io.LimitReader(target, item.Size+1)}, make([]byte, 256<<10))
	if err != nil {
		return err
	}
	if n != item.Size || hex.EncodeToString(hash.Sum(nil)) != p.Digest {
		return ErrState
	}
	after, err := target.Stat()
	if err != nil || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) || info.Size() != after.Size() {
		return ErrState
	}
	named, err := openRegular(parent, name)
	if err != nil {
		return err
	}
	namedInfo, err := named.Stat()
	named.Close()
	if err != nil || Identity(namedInfo) != p.Identity {
		return ErrState
	}
	if err := validateDestinationBinding(d, item, p); err != nil {
		return err
	}
	return parent.Sync()
}

// Cleanup is optional and only touches this job's verified private staging name.
// It must run after successful state/history persistence, never as rollback.
func Cleanup(d Definition, item Entry, p Proof) error {
	if !strings.HasPrefix(p.Temp, ".nastool-copy-") || filepath.Base(p.Temp) != p.Temp || p.Identity == "" {
		return ErrPath
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, _, err := anchoredParent(root, item.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	stageFD, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stage := os.NewFile(uintptr(stageFD), p.Temp)
	defer stage.Close()
	stageInfo, err := stage.Stat()
	if err != nil || Identity(stageInfo) != p.StagingIdentity {
		return ErrState
	}
	stat, err := lstatAt(stage, "payload")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if statIdentity(stat) != p.Identity || stat.Mode&unix.S_IFMT != unix.S_IFREG && stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			return ErrState
		}
		if (d.Mode == "softlink") != (stat.Mode&unix.S_IFMT == unix.S_IFLNK) {
			return ErrState
		}
		if err = unix.Unlinkat(stageFD, "payload", 0); err != nil {
			return err
		}
	}
	if err = unix.Unlinkat(int(parent.Fd()), p.Temp, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}

func TempName(job string, index int) string {
	return fmt.Sprintf(".nastool-copy-%s-%d.tmp", job, index)
}
