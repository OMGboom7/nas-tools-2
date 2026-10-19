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
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var seasonArtworkName = regexp.MustCompile(`^season[0-9]{2}-(poster|banner|landscape)$`)

// Capture inode/size/mtime/ctime (not atime, which our hash read can update).
// This surrounds the full digest verification and detects subsequent content,
// link-count or permission changes during sidecar preparation/publication.
func sidecarMediaStamp(job Job, item JobItem) (string, error) {
	if err := validateDestinationBinding(job.Definition, item.Entry, item.Proof); err != nil {
		return "", err
	}
	d := job.Definition
	rootPath, rootIdentity, relative := d.TargetRoot, d.TargetIdentity, item.Target
	if d.Mode == "softlink" {
		rootPath, rootIdentity, relative = d.SourceRoot, d.SourceIdentity, item.Source
	}
	root, err := anchoredRoot(rootPath, rootIdentity)
	if err != nil {
		return "", err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, relative, false)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	f, err := openRegular(parent, leaf)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", err
	}
	if d.Mode == "link" || d.Mode == "softlink" {
		if err := validateSourceBinding(d, item.Entry); err != nil {
			return "", err
		}
	}
	return Digest([]any{Identity(info), info.Size(), info.ModTime().UnixNano(), [2]int64{st.Ctim.Sec, st.Ctim.Nsec}, st.Nlink}), nil
}

func sidecarTarget(item JobItem, scope, name string) (string, error) {
	if item.Kind != "media" || filepath.Base(name) != name || !validRelative(name) || name == "." {
		return "", ErrPath
	}
	dir := filepath.Dir(item.Target)
	stem := strings.TrimSuffix(filepath.Base(item.Target), filepath.Ext(item.Target))
	tv := item.MediaType == "电视剧" || item.MediaType == "动漫"
	ext := strings.ToLower(filepath.Ext(name))
	base := strings.TrimSuffix(name, filepath.Ext(name))
	valid := false
	if scope == "show" {
		if !tv {
			return "", ErrPath
		}
		dir = filepath.Dir(dir)
	} else if scope != "media" {
		return "", ErrPath
	}
	if ext == ".nfo" {
		valid = scope == "media" && (base == stem || tv && base == "season") || scope == "show" && base == "tvshow"
	} else if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		if scope == "media" && tv {
			valid = base == stem+"-thumb"
		} else {
			valid = map[string]bool{"poster": true, "fanart": true, "background": true, "logo": true, "disc": !tv, "clearart": tv, "banner": true, "thumb": true}[base] || tv && seasonArtworkName.MatchString(base)
		}
	}
	target := filepath.Join(dir, name)
	if !valid || !validRelative(target) {
		return "", ErrPath
	}
	return target, nil
}

// PublishSidecar creates only missing metadata beside a proven completed media
// item. All path components are descriptor-relative and no leaf is truncated.
// prepare must commit the private object's identity before writing any bytes.
func PublishSidecar(ctx context.Context, job Job, item JobItem, scope, name string, data []byte, prepare func(SidecarProof) error, beforePublish func() error) (proof SidecarProof, state string, err error) {
	state = "blocked"
	if len(data) == 0 || len(data) > 16<<20 || prepare == nil || beforePublish == nil || job.State != "completed" || item.State != "completed" || item.Reason != "" {
		return proof, state, ErrState
	}
	target, err := sidecarTarget(item, scope, name)
	if err != nil {
		return proof, state, err
	}
	if err := ctx.Err(); err != nil {
		return proof, state, err
	}
	mediaStamp, err := sidecarMediaStamp(job, item)
	if err != nil {
		return proof, state, err
	}
	if err := VerifyCompletedTarget(ctx, job, item); err != nil {
		return proof, state, err
	}
	if after, err := sidecarMediaStamp(job, item); err != nil || after != mediaStamp {
		return proof, state, ErrState
	}
	if err := validateDestinationBinding(job.Definition, item.Entry, item.Proof); err != nil {
		return proof, state, err
	}
	root, err := anchoredRoot(job.Definition.TargetRoot, job.Definition.TargetIdentity)
	if err != nil {
		return proof, state, err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, target, false)
	if err != nil {
		return proof, state, err
	}
	defer parent.Close()
	pi, err := parent.Stat()
	if err != nil {
		return proof, state, err
	}
	proof.Target, proof.ParentIdentity, proof.Size = target, Identity(pi), int64(len(data))
	hash := sha256.Sum256(data)
	proof.Digest = hex.EncodeToString(hash[:])
	var existing unix.Stat_t
	if item.MediaType == "电影" && strings.HasSuffix(name, ".nfo") {
		if err := unix.Fstatat(int(parent.Fd()), "movie.nfo", &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return proof, "preserved", nil
		} else if !errors.Is(err, unix.ENOENT) {
			return proof, state, err
		}
	}
	if err := unix.Fstatat(int(parent.Fd()), leaf, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return proof, "preserved", nil
	} else if !errors.Is(err, unix.ENOENT) {
		return proof, state, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return proof, state, err
	}
	proof.Temp = ".nastool-sidecar-" + hex.EncodeToString(nonce[:])
	if err := unix.Mkdirat(int(parent.Fd()), proof.Temp, 0700); err != nil {
		return proof, state, err
	}
	stageFD, err := unix.Openat(int(parent.Fd()), proof.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return proof, state, err
	}
	stage := os.NewFile(uintptr(stageFD), proof.Temp)
	defer stage.Close()
	stageInfo, err := stage.Stat()
	if err != nil {
		return proof, state, err
	}
	var stageStat unix.Stat_t
	if unix.Fstat(stageFD, &stageStat) != nil || stageInfo.Mode().Perm() != 0700 || int(stageStat.Uid) != os.Geteuid() {
		return proof, state, ErrPath
	}
	proof.StagingIdentity = Identity(stageInfo)
	defer func() {
		current, _, e := anchoredParent(root, filepath.Join(filepath.Dir(target), proof.Temp, "payload"), false)
		if e == nil {
			defer current.Close()
			info, e := current.Stat()
			if e == nil && Identity(info) == proof.StagingIdentity {
				_ = unix.Unlinkat(int(parent.Fd()), proof.Temp, unix.AT_REMOVEDIR)
			}
		}
	}()
	fd, err := unix.Openat(stageFD, "payload", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return proof, state, err
	}
	f := os.NewFile(uintptr(fd), proof.Temp)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return proof, state, err
	}
	proof.Identity = Identity(fi)
	defer func() {
		var st, own unix.Stat_t
		if unix.Fstatat(stageFD, "payload", &st, unix.AT_SYMLINK_NOFOLLOW) == nil && unix.Fstat(int(f.Fd()), &own) == nil && st.Ino == own.Ino && st.Dev == own.Dev {
			_ = unix.Unlinkat(stageFD, "payload", 0)
		}
	}()
	if err := prepare(proof); err != nil {
		return proof, state, err
	}
	if err := ctx.Err(); err != nil {
		return proof, state, err
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
	if err := beforePublish(); err != nil {
		return proof, state, err
	}
	check := func() error {
		if stamp, err := sidecarMediaStamp(job, item); err != nil || stamp != mediaStamp {
			return ErrState
		}
		current, err := anchoredRoot(job.Definition.TargetRoot, job.Definition.TargetIdentity)
		if err != nil {
			return err
		}
		defer current.Close()
		cp, _, err := anchoredParent(current, target, false)
		if err != nil {
			return err
		}
		defer cp.Close()
		ci, err := cp.Stat()
		if err != nil || Identity(ci) != proof.ParentIdentity {
			return ErrPath
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return proof, state, err
	}
	if err := check(); err != nil {
		return proof, state, err
	}
	currentStage, _, err := anchoredParent(root, filepath.Join(filepath.Dir(target), proof.Temp, "payload"), false)
	if err != nil {
		return proof, state, err
	}
	defer currentStage.Close()
	currentStageInfo, err := currentStage.Stat()
	if err != nil || Identity(currentStageInfo) != proof.StagingIdentity {
		return proof, state, ErrState
	}
	preparedFile, err := openRegular(stage, "payload")
	if err != nil {
		return proof, state, err
	}
	defer preparedFile.Close()
	preparedInfo, err := preparedFile.Stat()
	var preparedStat unix.Stat_t
	if err != nil || Identity(preparedInfo) != proof.Identity || preparedInfo.Size() != proof.Size || unix.Fstat(int(preparedFile.Fd()), &preparedStat) != nil || preparedStat.Nlink != 1 {
		return proof, state, ErrState
	}
	digest, err := hashSource(ctx, preparedFile, proof.Size)
	if err != nil || digest != proof.Digest {
		return proof, state, ErrState
	}
	if item.MediaType == "电影" && strings.HasSuffix(name, ".nfo") {
		if err := unix.Fstatat(int(parent.Fd()), "movie.nfo", &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return proof, "preserved", nil
		} else if !errors.Is(err, unix.ENOENT) {
			return proof, state, err
		}
	}
	if err := renameNoReplace(stageFD, "payload", int(parent.Fd()), leaf); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return proof, "preserved", nil
		}
		return proof, state, err
	}
	state = "uncertain"
	if err := parent.Sync(); err != nil {
		return proof, state, err
	}
	if err := check(); err != nil {
		return proof, state, err
	}
	actual, err := openRegular(parent, leaf)
	if err != nil {
		return proof, state, err
	}
	defer actual.Close()
	ai, err := actual.Stat()
	if err != nil || Identity(ai) != proof.Identity || ai.Size() != proof.Size {
		return proof, state, ErrState
	}
	return proof, "created", nil
}
