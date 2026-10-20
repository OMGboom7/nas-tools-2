//go:build linux || darwin

package organization

import (
	"context"
	"os"
)

// Sidecar staging changes the disc root's timestamps, but must not change any
// approved payload node. Mutable, bounded root metadata is checked separately
// by the completed-target verifier before staging starts.
func discMediaStamp(ctx context.Context, job Job, item JobItem) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	d, e, p := job.Definition, item.Entry, item.Proof
	if err := validateDestinationBinding(d, e, p); err != nil {
		return "", err
	}
	if err := discProofValid(d, e, p); err != nil {
		return "", err
	}
	parent, name, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	root, err := discDir(parent, name)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Stat()
	if err != nil || Identity(info) != p.Identity {
		return "", ErrState
	}
	m, _ := discManifest(d, e)
	nodes := []LibraryNode{}
	files := discFiles(p)
	var source *os.File
	if d.Mode == "softlink" {
		source, err = discSource(ctx, d, e)
		if err != nil {
			return "", err
		}
		defer source.Close()
	}
	for _, n := range m.Nodes {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !n.Directory && discMetadata(e, n.Path) {
			continue
		}
		node, _, err := discLeaf(root, n.Path)
		if err != nil {
			return "", err
		}
		expected := files[n.Path].Identity
		if n.Directory {
			expected = p.Disc.Directories[n.Path]
		}
		if node.Identity != expected {
			return "", ErrState
		}
		nodes = append(nodes, node)
		if source != nil && !n.Directory {
			original, _, err := discLeaf(source, n.Path)
			if err != nil || original.Identity != n.Node.Identity {
				return "", ErrState
			}
			nodes = append(nodes, original)
		}
	}
	return Digest([]any{p.Identity, nodes}), nil
}
