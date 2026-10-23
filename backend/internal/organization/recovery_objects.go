package organization

import (
	"context"
	"encoding/hex"
	"path"
	"sort"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

const RecoveryClosed = "recovery_closed"
const RecoveryObjectLimit = 20000
const RecoveryByteLimit int64 = 1 << 50

// A recovery disposal is a new, independently reviewed destructive operation.
// Its snapshot is never adopted as a missing proof of an earlier transfer.
type RecoveryScope struct {
	Root         LibraryRoot          `json:"root"`
	Remote       *remotestore.Binding `json:"remote,omitempty"`
	ConfigDigest string               `json:"configDigest"`
}
type RecoveryNode struct {
	Path   string            `json:"path"`
	Kind   string            `json:"kind"`
	Node   LibraryNode       `json:"node"`
	Remote *remotestore.Node `json:"remote,omitempty"`
	Digest string            `json:"digest,omitempty"`
	Link   string            `json:"link,omitempty"`
	Mode   uint32            `json:"mode"`
}
type RecoveryOwner struct {
	BatchID     string `json:"batchId,omitempty"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Related     bool   `json:"related"`
}
type RecoveryDefinition struct {
	Removal   *MediaRemovalOrigin `json:"removal,omitempty"`
	Scope     RecoveryScope       `json:"scope"`
	Path      string              `json:"path"`
	Nodes     []RecoveryNode      `json:"nodes"`
	Owners    []RecoveryOwner     `json:"owners"`
	Actor     string              `json:"actor"`
	RequestID string              `json:"requestId"`
	Decision  string              `json:"decision"`
}

// Only the separately reviewed media-removal coordinator may create these
// children. Public recovery-object endpoints never grant ordinary-media scope.
type MediaRemovalOrigin struct {
	Parent string `json:"parent"`
	Role   string `json:"role"`
}

func recoveryDefinitionPath(d RecoveryDefinition) bool {
	if d.Removal == nil {
		return RecoveryCandidate(d.Path)
	}
	return recoveryHex(d.Removal.Parent, 64) && (d.Removal.Role == "source" || d.Removal.Role == "library" || d.Removal.Role == "file" || d.Removal.Role == "cleanup") && validRelative(d.Path) && d.Path != "."
}

func MediaRemovalSnapshot(ctx context.Context, s RecoveryScope, selected string, c *remotestore.Client) ([]RecoveryNode, error) {
	if !validRelative(selected) || selected == "." || recoveryClient(s, c) != nil {
		return nil, ErrPath
	}
	if s.Remote != nil {
		return recoveryRemoteSnapshot(ctx, s, selected, c)
	}
	return recoveryLocalSnapshot(ctx, s, selected)
}

type RecoveryObjectReview struct {
	RootID      string             `json:"rootId"`
	Driver      string             `json:"driver"`
	Path        string             `json:"path"`
	RequestID   string             `json:"requestId"`
	Fingerprint string             `json:"fingerprint"`
	Files       int                `json:"files"`
	Directories int                `json:"directories"`
	Links       int                `json:"links"`
	Bytes       int64              `json:"bytes"`
	Unknown     bool               `json:"unknown"`
	Owners      []RecoveryOwner    `json:"owners"`
	Sample      []string           `json:"sample"`
	Definition  RecoveryDefinition `json:"-"`
}
type RecoveryDisposal struct {
	ID         string                 `json:"id"`
	State      string                 `json:"state"`
	RootID     string                 `json:"rootId"`
	Driver     string                 `json:"driver"`
	Path       string                 `json:"path"`
	Capture    string                 `json:"capture,omitempty"`
	Actor      string                 `json:"actor"`
	Created    string                 `json:"created"`
	Updated    string                 `json:"updated"`
	Reason     string                 `json:"reason"`
	Decision   string                 `json:"decision"`
	Total      int                    `json:"total"`
	Done       int                    `json:"done"`
	Definition RecoveryDefinition     `json:"-"`
	Items      []RecoveryDisposalItem `json:"-"`
	Frozen     []RecoveryOwner        `json:"-"`
	LastReview string                 `json:"-"`
	ReturnNode *RecoveryNode          `json:"-"`
}
type RecoveryDisposalItem struct {
	Ordinal int
	State   string
	Node    RecoveryNode
}
type RecoveryDisposalReview struct {
	Disposal    RecoveryDisposal `json:"disposal"`
	Fingerprint string           `json:"fingerprint"`
	CanContinue bool             `json:"canContinue"`
	CanReturn   bool             `json:"canReturn"`
	Evidence    string           `json:"evidence"`
	ReturnNode  *RecoveryNode    `json:"-"`
}
type RecoveryBrowseEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Candidate bool   `json:"candidate"`
}

func recoveryHex(value string, length int) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(value) == length && hex.EncodeToString(b) == value
}
func recoveryScopeValid(s RecoveryScope) bool {
	if s.Root.ID == "" || s.Root.Path == "/" || !path.IsAbs(s.Root.Path) || path.Clean(s.Root.Path) != s.Root.Path || strings.ContainsAny(s.Root.Path, "\\\x00\r\n") || !recoveryHex(s.ConfigDigest, 64) {
		return false
	}
	if s.Remote != nil {
		return s.Remote.Protocol == 1 && LibraryRemoteMode(s.Remote.Driver) != "" && s.Root.Identity == RemoteTargetIdentityFor(LibraryRemoteMode(s.Remote.Driver), s.Root.Path)
	}
	return s.Root.Identity != ""
}
func recoveryClient(s RecoveryScope, c *remotestore.Client) error {
	if !recoveryScopeValid(s) {
		return ErrPath
	}
	if s.Remote == nil {
		if c != nil {
			return ErrState
		}
		return nil
	}
	if c == nil || c.Binding() != *s.Remote {
		return ErrState
	}
	return c.Current()
}

// Only a whole private recovery unit can be selected. Its name is not proof
// of ownership. Even foreign children must be shown in the new full snapshot.
func RecoveryCandidate(relative string) bool {
	if !validRelative(relative) || relative == "." {
		return false
	}
	parts := strings.Split(relative, "/")
	leaf := path.Base(relative)
	for _, prefix := range []string{".nastool-copy-", ".nastool-move-", ".nastool-library-", ".nastool-sidecar-", ".nastool-dispose-"} {
		if strings.HasPrefix(leaf, prefix) && len(leaf) > len(prefix) && leaf != ".nastool-library-remote" {
			return true
		}
	}
	if len(parts) < 2 || !recoveryHex(leaf, 32) {
		return false
	}
	switch parts[len(parts)-2] {
	case ".nastool-remote", ".nastool-remote-sidecars", ".nastool-library-remote":
		return true
	}
	return false
}
func recoverySort(nodes []RecoveryNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
}
func recoveryNodeSame(a, b RecoveryNode) bool { return Digest(a) == Digest(b) }
func recoveryCapture(id, selected string) string {
	return path.Join(path.Dir(selected), ".nastool-dispose-"+id)
}
func recoveryDriver(s RecoveryScope) string {
	if s.Remote != nil {
		return s.Remote.Driver
	}
	return ""
}

func RecoverySnapshot(ctx context.Context, s RecoveryScope, selected string, c *remotestore.Client) ([]RecoveryNode, error) {
	if !RecoveryCandidate(selected) || recoveryClient(s, c) != nil {
		return nil, ErrPath
	}
	if s.Remote != nil {
		return recoveryRemoteSnapshot(ctx, s, selected, c)
	}
	return recoveryLocalSnapshot(ctx, s, selected)
}
func RecoveryBrowse(ctx context.Context, s RecoveryScope, relative string, c *remotestore.Client) ([]RecoveryBrowseEntry, error) {
	if !validRelative(relative) || recoveryClient(s, c) != nil {
		return nil, ErrPath
	}
	if s.Remote != nil {
		return recoveryRemoteBrowse(ctx, s, relative, c)
	}
	return recoveryLocalBrowse(ctx, s, relative)
}
