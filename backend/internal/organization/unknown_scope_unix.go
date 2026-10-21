//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"strconv"
)

// Completion covers the saved source scope, not just a successful subset.
func verifyUnknownScope(ctx context.Context, job Job) error {
	d := job.Definition
	if !validRelative(d.UnknownScope) || d.UnknownScopeIdentity == "" {
		return ErrState
	}
	anchor, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer anchor.Close()
	root, err := OpenRoot(d.SourceRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(d.UnknownScope)
	if errors.Is(err, os.ErrNotExist) && MoveMode(d.Mode) {
		for _, item := range job.Items {
			if IsDisc(item.Entry) && item.Source == d.UnknownScope && item.Identity == d.UnknownScopeIdentity && item.State == "completed" && item.Reason == "" {
				return VerifyCompletedTarget(ctx, job, item)
			}
		}
	}
	if errors.Is(err, os.ErrNotExist) && !d.UnknownDirectory && MoveMode(d.Mode) {
		return nil
	}
	if err != nil || Identity(info) != d.UnknownScopeIdentity {
		return ErrState
	}
	scan, err := Scan(ctx, d.SourceRoot, d.UnknownScope)
	if err != nil {
		return err
	}
	if scan.Skipped != 0 {
		return ErrState
	}
	known := map[string]JobItem{}
	for _, item := range job.Items {
		known[item.Source] = item
	}
	for _, file := range scan.Files {
		item, ok := known[file.Path]
		if !ok || item.Identity != file.Identity || item.Size != file.Size || item.Modified != strconv.FormatInt(file.Modified, 10) || item.Kind != file.Kind || item.Proof.Incomplete || len(item.Proof.Digest) != 64 {
			return ErrState
		}
		if IsDisc(item.Entry) {
			if _, err := discSourceStamp(ctx, d, item.Entry, &item.Proof, true, true); err != nil {
				return err
			}
			continue
		}
		parent, name, err := anchoredParent(anchor, item.Source, false)
		if err != nil {
			return err
		}
		err = verifyOriginal(ctx, parent, name, item.Entry, item.Proof.Digest)
		parent.Close()
		if err != nil {
			return err
		}
	}
	info, err = root.Lstat(d.UnknownScope)
	if err != nil || Identity(info) != d.UnknownScopeIdentity {
		return ErrState
	}
	return nil
}
