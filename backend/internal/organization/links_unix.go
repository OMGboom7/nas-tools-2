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
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func sameDevice(a, b string) bool {
	aa, bb := strings.Split(a, ":"), strings.Split(b, ":")
	return len(aa) == 2 && len(bb) == 2 && aa[0] != "" && aa[0] == bb[0]
}

func modeOperationError(err error) error {
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		return ErrMode
	}
	return err
}

// ValidateMode is read-only. For hard links, the nearest existing target
// ancestor determines the device of directories we will create later.
func ValidateMode(ctx context.Context, d Definition, item Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !SupportedMode(d.Mode) {
		return ErrMode
	}
	if d.Mode == "softlink" && len(filepath.Join(d.SourceRoot, item.Source)) > 4096 {
		return ErrMode
	}
	if d.Mode != "link" {
		return nil
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	if !validRelative(item.Target) || item.Target == "." {
		return ErrPath
	}
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return err
	}
	unix.CloseOnExec(fd)
	parts := strings.Split(item.Target, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			unix.Close(fd)
			return err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil {
			unix.Close(fd)
			return ErrPath
		}
		unix.Close(fd)
		fd = next
	}
	parent := os.NewFile(uintptr(fd), "target ancestor")
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		return err
	}
	if !sameDevice(item.Identity, Identity(info)) {
		return ErrMode
	}
	return nil
}

func lstatAt(parent *os.File, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if filepath.Base(name) != name || !validRelative(name) {
		return stat, ErrPath
	}
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return stat, err
}
func statIdentity(stat unix.Stat_t) string { return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino) }

func readLinkAt(parent *os.File, name string) (string, error) {
	if filepath.Base(name) != name || !validRelative(name) {
		return "", ErrPath
	}
	buf := make([]byte, 4097)
	n, err := unix.Readlinkat(int(parent.Fd()), name, buf)
	if err != nil || n > 4096 {
		return "", ErrPath
	}
	return string(buf[:n]), nil
}

func hashSource(ctx context.Context, source *os.File, size int64) (string, error) {
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, contextReader{ctx, io.LimitReader(source, size+1)}, make([]byte, 256<<10))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrPath
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func preparePayload(ctx context.Context, d Definition, item Entry, source *os.File, sourceInfo os.FileInfo, sourceParent *os.File, sourceName string, stage *os.File) (Proof, error) {
	proof := Proof{Kind: "regular"}
	stageFD := int(stage.Fd())
	switch d.Mode {
	case "copy":
		fd, err := unix.Openat(stageFD, "payload", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return proof, ErrState
		}
		temp := os.NewFile(uintptr(fd), "payload")
		defer temp.Close()
		hash := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(temp, hash), contextReader{ctx, io.LimitReader(source, item.Size+1)}, make([]byte, 256<<10))
		if err != nil {
			return proof, err
		}
		if n != item.Size {
			return proof, ErrPath
		}
		if err = temp.Chmod(sourceInfo.Mode().Perm() & 0777); err != nil {
			return proof, err
		}
		if err = unix.Futimes(fd, []unix.Timeval{unix.NsecToTimeval(time.Now().UnixNano()), unix.NsecToTimeval(sourceInfo.ModTime().UnixNano())}); err != nil {
			return proof, err
		}
		if err = temp.Sync(); err != nil {
			return proof, err
		}
		info, err := temp.Stat()
		if err != nil {
			return proof, err
		}
		proof.Identity, proof.Digest = Identity(info), hex.EncodeToString(hash.Sum(nil))
	case "link":
		// Link the name without following symlinks, then verify that the linked
		// inode is the regular source descriptor whose snapshot was approved.
		if err := unix.Linkat(int(sourceParent.Fd()), sourceName, stageFD, "payload", 0); err != nil {
			return proof, modeOperationError(err)
		}
		anchor, err := openRegular(stage, "payload")
		if err != nil {
			return proof, ErrPath
		}
		defer anchor.Close()
		info, err := anchor.Stat()
		if err != nil || !matches(info, item) {
			return proof, ErrPath
		}
		proof.Identity = Identity(info)
		proof.Digest, err = hashSource(ctx, source, item.Size)
		if err != nil {
			return proof, err
		}
		if err = anchor.Sync(); err != nil {
			return proof, err
		}
	case "softlink":
		linkTarget := filepath.Join(d.SourceRoot, item.Source)
		if len(linkTarget) > 4096 {
			return proof, ErrMode
		}
		// The absolute link is intentionally outside the library, but only to
		// the stored, administrator-approved source root. Never follow it for
		// verification: re-open the source using pinned no-follow descriptors.
		if err := unix.Symlinkat(linkTarget, stageFD, "payload"); err != nil {
			return proof, modeOperationError(err)
		}
		stat, err := lstatAt(stage, "payload")
		if err != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			return proof, ErrPath
		}
		proof.Kind, proof.LinkTarget, proof.Identity = "symlink", linkTarget, statIdentity(stat)
		proof.Digest, err = hashSource(ctx, source, item.Size)
		if err != nil {
			return proof, err
		}
	default:
		return proof, ErrMode
	}
	return proof, nil
}

func verifyLinkObject(parent *os.File, name string, p Proof) error {
	stat, err := lstatAt(parent, name)
	if err != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK || statIdentity(stat) != p.Identity {
		return ErrState
	}
	target, err := readLinkAt(parent, name)
	if err != nil || target != p.LinkTarget {
		return ErrState
	}
	return nil
}

func verifySoftlink(ctx context.Context, d Definition, item Entry, p Proof, parent *os.File, name string, stage *os.File) error {
	if p.Kind != "symlink" || p.LinkTarget != filepath.Join(d.SourceRoot, item.Source) {
		return ErrState
	}
	if err := verifyLinkObject(stage, "payload", p); err != nil {
		return err
	}
	if err := verifyLinkObject(parent, name, p); err != nil {
		return err
	}
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	sourceParent, sourceName, err := anchoredParent(root, item.Source, false)
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	source, err := openRegular(sourceParent, sourceName)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !matches(info, item) {
		return ErrPath
	}
	digest, err := hashSource(ctx, source, item.Size)
	if err != nil {
		return err
	}
	if digest != p.Digest {
		return ErrState
	}
	after, err := source.Stat()
	if err != nil || !matches(after, item) {
		return ErrPath
	}
	current, err := openRegular(sourceParent, sourceName)
	if err != nil {
		return ErrPath
	}
	currentInfo, err := current.Stat()
	current.Close()
	if err != nil || !matches(currentInfo, item) {
		return ErrPath
	}
	// Check the link again after the source query; do not count a replaced or
	// dangling link as successful just because the approved source was valid.
	if err := verifyLinkObject(parent, name, p); err != nil {
		return err
	}
	if err := validateSourceBinding(d, item); err != nil {
		return err
	}
	if err := validateDestinationBinding(d, item, p); err != nil {
		return err
	}
	return parent.Sync()
}

func validateSourceBinding(d Definition, item Entry) error {
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, name, err := anchoredParent(root, item.Source, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	f, err := openRegular(parent, name)
	if err != nil {
		return ErrPath
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !matches(info, item) {
		return ErrPath
	}
	return nil
}

func validateDestinationBinding(d Definition, item Entry, p Proof) error {
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
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	if d.Mode == "softlink" {
		return verifyLinkObject(parent, name, p)
	}
	f, err := openRegular(parent, name)
	if err != nil {
		return ErrState
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || Identity(info) != p.Identity || info.Size() != item.Size {
		return ErrState
	}
	return nil
}
