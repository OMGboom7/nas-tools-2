package organization

import (
	"context"
	"path"
	"sort"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func recoveryRemoteNode(relative string, n remotestore.Node) RecoveryNode {
	n.Path, n.Name = path.Base(relative), path.Base(relative)
	r := RecoveryNode{Path: relative, Kind: "file", Node: LibraryNode{Size: n.Size}, Remote: &n}
	if n.IsDir {
		r.Kind = "directory"
		n.ID = ""
		n.ModTime = ""
		n.Size = 0
		r.Node.Size = 0
	}
	return r
}
func recoveryRemoteInventory(ctx context.Context, s RecoveryScope, selected string, c *remotestore.Client) ([]RecoveryNode, error) {
	if err := recoveryClient(s, c); err != nil {
		return nil, err
	}
	n, err := c.Entry(ctx, s.Root.Path, selected)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, ErrPath
	}
	root := recoveryRemoteNode(".", *n)
	// Entry and recursive list spell the selected directory differently; the
	// capability remains its exact full path, not these display-only leaf names.
	root.Remote.Path, root.Remote.Name = ".", "."
	nodes := []RecoveryNode{root}
	if n.IsDir {
		children, err := c.List(ctx, path.Join(s.Root.Path, selected), true)
		if err != nil {
			return nil, err
		}
		if len(children)+1 > RecoveryObjectLimit {
			return nil, ErrLimit
		}
		for _, child := range children {
			if !validRelative(child.Path) || child.Path == "." {
				return nil, ErrPath
			}
			nodes = append(nodes, recoveryRemoteNode(child.Path, child))
		}
	}
	recoverySort(nodes)
	var bytes int64
	for _, node := range nodes {
		if node.Kind == "directory" {
			continue
		}
		if node.Node.Size < 0 || node.Node.Size > RecoveryByteLimit-bytes {
			return nil, ErrLimit
		}
		bytes += node.Node.Size
	}
	return nodes, nil
}
func recoveryRemoteSnapshot(ctx context.Context, s RecoveryScope, selected string, c *remotestore.Client) ([]RecoveryNode, error) {
	nodes, err := recoveryRemoteInventory(ctx, s, selected, c)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].Kind == "directory" {
			continue
		}
		if nodes[i].Node.Size < 0 || nodes[i].Node.Size > RecoveryByteLimit {
			return nil, ErrLimit
		}
		nodes[i].Digest, err = libraryRemoteHash(ctx, c, path.Join(s.Root.Path, selected, nodes[i].Path), nodes[i].Node.Size)
		if err != nil {
			return nil, err
		}
	}
	after, err := recoveryRemoteInventory(ctx, s, selected, c)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if i >= len(after) {
			return nil, ErrState
		}
		after[i].Digest = nodes[i].Digest
	}
	if Digest(nodes) != Digest(after) {
		return nil, ErrState
	}
	return nodes, nil
}
func recoveryRemoteBrowse(ctx context.Context, s RecoveryScope, relative string, c *remotestore.Client) ([]RecoveryBrowseEntry, error) {
	nodes, err := c.List(ctx, path.Join(s.Root.Path, relative), false)
	if err != nil {
		return nil, err
	}
	entries := make([]RecoveryBrowseEntry, 0, len(nodes))
	for _, n := range nodes {
		p := path.Join(relative, n.Path)
		entries = append(entries, RecoveryBrowseEntry{Name: n.Name, Path: p, Directory: n.IsDir, Candidate: RecoveryCandidate(p)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}
