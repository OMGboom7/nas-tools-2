package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"gopkg.in/yaml.v3"
)

var ErrInvalidConfigPath = errors.New("invalid configuration path")

// Store performs comment-preserving, atomic updates to config.yaml. It reads
// the file for every operation so changes made by an older process during the
// migration window are not overwritten from a stale in-memory snapshot.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (store *Store) Snapshot() (map[string]any, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	root, _, _, err := store.read()
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	if err := root.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode application config: %w", err)
	}
	return result, nil
}

func (store *Store) Update(items map[string]any) error {
	if len(items) == 0 {
		return errors.New("at least one configuration item is required")
	}
	paths := make([]string, 0, len(items))
	for path := range items {
		if _, err := splitConfigPath(path); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)

	store.mu.Lock()
	defer store.mu.Unlock()
	root, original, mode, err := store.read()
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := setNodePath(root, path, items[path]); err != nil {
			return err
		}
	}
	return store.write(root, original, mode)
}

func (store *Store) UpdateDirectory(operation, path, value, replacement string) error {
	if operation != "add" && operation != "sub" && operation != "set" {
		return errors.New("unsupported directory operation")
	}
	if value == "" {
		return errors.New("directory value is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	root, original, mode, err := store.read()
	if err != nil {
		return err
	}
	node, err := findNodePath(root, path, operation == "add")
	if err != nil {
		return err
	}
	values := make([]string, 0)
	if node != nil && node.Kind != 0 && node.Tag != "!!null" {
		if node.Kind == yaml.SequenceNode {
			if err := node.Decode(&values); err != nil {
				return fmt.Errorf("decode directory list: %w", err)
			}
		} else {
			var existing string
			if err := node.Decode(&existing); err != nil {
				return fmt.Errorf("decode directory value: %w", err)
			}
			if existing != "" {
				values = append(values, existing)
			}
		}
	}
	value = filepath.ToSlash(value)
	replacement = filepath.ToSlash(replacement)
	switch operation {
	case "add":
		values = append(values, value)
	case "sub", "set":
		filtered := values[:0]
		for _, existing := range values {
			if directoryIdentity(existing) != directoryIdentity(value) {
				filtered = append(filtered, existing)
			}
		}
		values = filtered
		if operation == "set" && replacement != "" {
			values = append(values, replacement)
		}
	}
	var updated any = values
	if len(values) == 0 {
		updated = nil
	}
	if err := setNodePath(root, path, updated); err != nil {
		return err
	}
	return store.write(root, original, mode)
}

func (store *Store) read() (*yaml.Node, []byte, os.FileMode, error) {
	original, err := os.ReadFile(store.path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read application config: %w", err)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("stat application config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil {
		return nil, nil, 0, fmt.Errorf("parse application config: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, 0, errors.New("application config root must be a mapping")
	}
	return document.Content[0], original, info.Mode().Perm(), nil
}

func (store *Store) write(root *yaml.Node, original []byte, mode os.FileMode) error {
	document := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	updated, err := yaml.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode application config: %w", err)
	}
	if err := atomicWrite(store.path+".bak", original, mode); err != nil {
		return fmt.Errorf("backup application config: %w", err)
	}
	if err := atomicWrite(store.path, updated, mode); err != nil {
		return fmt.Errorf("write application config: %w", err)
	}
	return nil
}

func atomicWrite(path string, contents []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".nastool-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func setNodePath(root *yaml.Node, path string, value any) error {
	parent, key, err := findNodeParent(root, path, true)
	if err != nil {
		return err
	}
	updated := &yaml.Node{}
	if err := updated.Encode(value); err != nil {
		return fmt.Errorf("encode configuration value %q: %w", path, err)
	}
	for index := 0; index < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			old := parent.Content[index+1]
			updated.HeadComment = old.HeadComment
			updated.LineComment = old.LineComment
			updated.FootComment = old.FootComment
			parent.Content[index+1] = updated
			return nil
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, updated)
	return nil
}

func findNodePath(root *yaml.Node, path string, create bool) (*yaml.Node, error) {
	parent, key, err := findNodeParent(root, path, create)
	if err != nil || parent == nil {
		return nil, err
	}
	for index := 0; index < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			return parent.Content[index+1], nil
		}
	}
	return nil, nil
}

func findNodeParent(root *yaml.Node, path string, create bool) (*yaml.Node, string, error) {
	parts, err := splitConfigPath(path)
	if err != nil {
		return nil, "", err
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		if current.Kind != yaml.MappingNode {
			return nil, "", fmt.Errorf("configuration parent %q is not a mapping", part)
		}
		var child *yaml.Node
		for index := 0; index < len(current.Content); index += 2 {
			if current.Content[index].Value == part {
				child = current.Content[index+1]
				break
			}
		}
		if child == nil {
			if !create {
				return nil, parts[len(parts)-1], nil
			}
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, child)
		}
		if child.Kind != yaml.MappingNode {
			if !create {
				return nil, "", fmt.Errorf("configuration parent %q is not a mapping", part)
			}
			child.Kind, child.Tag, child.Value, child.Content = yaml.MappingNode, "!!map", "", nil
		}
		current = child
	}
	return current, parts[len(parts)-1], nil
}

func splitConfigPath(path string) ([]string, error) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 || len(parts) > 8 {
		return nil, ErrInvalidConfigPath
	}
	for _, part := range parts {
		if part == "" || len(part) > 128 {
			return nil, ErrInvalidConfigPath
		}
		for _, character := range part {
			if unicode.IsControl(character) || unicode.IsSpace(character) {
				return nil, ErrInvalidConfigPath
			}
		}
	}
	return parts, nil
}

func directoryIdentity(value string) string {
	return strings.SplitN(filepath.ToSlash(value), "@", 2)[0]
}
