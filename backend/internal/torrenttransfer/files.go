package torrenttransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// MetainfoRoot owns only read access. A source snapshot can neither overwrite
// the original torrent/resume nor create or remove any payload file.
type MetainfoRoot struct {
	configured string
	root       *os.Root
	physical   string
	info       os.FileInfo
	identity   string
}
type FileProof struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
}
type Source struct {
	Hash         string                       `json:"hash"`
	Root         string                       `json:"root"`
	RootIdentity string                       `json:"rootIdentity"`
	Torrent      FileProof                    `json:"torrent"`
	Resume       *FileProof                   `json:"resume,omitempty"`
	Prepared     torrentmeta.TransferMetadata `json:"-"`
}

func OpenMetainfoRoot(name string) (*MetainfoRoot, error) {
	if len(name) > 4096 || !filepath.IsAbs(name) || filepath.Clean(name) == string(filepath.Separator) || strings.ContainsAny(name, "\x00\r\n") {
		return nil, ErrState
	}
	physical, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, ErrState
	}
	info, err := os.Lstat(physical)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrState
	}
	identity, err := fileIdentity(info)
	if err != nil {
		return nil, err
	}
	bound, err := os.OpenRoot(physical)
	if err != nil {
		return nil, ErrState
	}
	r := &MetainfoRoot{root: bound, configured: filepath.Clean(name), physical: physical, info: info, identity: identity}
	if err := r.check(); err != nil {
		bound.Close()
		return nil, err
	}
	return r, nil
}
func (r *MetainfoRoot) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	return r.root.Close()
}
func (r *MetainfoRoot) Path() string     { return r.physical }
func (r *MetainfoRoot) Identity() string { return r.identity }
func (r *MetainfoRoot) check() error {
	if r == nil || r.root == nil {
		return ErrState
	}
	resolved, err := filepath.EvalSymlinks(r.configured)
	if err != nil || resolved != r.physical {
		return ErrState
	}
	current, err := os.Lstat(r.physical)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(r.info, current) {
		return ErrState
	}
	bound, err := r.root.Stat(".")
	if err != nil || !os.SameFile(r.info, bound) {
		return ErrState
	}
	return nil
}
func sameFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && before.Mode().IsRegular() && after.Mode().IsRegular() && os.SameFile(before, after) && before.Size() == after.Size() && before.Mode() == after.Mode() && before.ModTime().Equal(after.ModTime())
}
func (r *MetainfoRoot) read(ctx context.Context, name string) ([]byte, FileProof, error) {
	proof := FileProof{}
	if ctx.Err() != nil || name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) || filepath.Base(name) != name || strings.ContainsAny(name, "\x00\r\n\\") || !strings.HasSuffix(name, ".torrent") && !strings.HasSuffix(name, ".fastresume") {
		return nil, proof, ErrState
	}
	if err := r.check(); err != nil {
		return nil, proof, err
	}
	before, err := r.root.Lstat(name)
	if err != nil {
		return nil, proof, err
	}
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 8<<20 {
		return nil, proof, ErrState
	}
	file, err := r.root.Open(name)
	if err != nil {
		return nil, proof, ErrState
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFile(before, opened) {
		return nil, proof, ErrState
	}
	content, err := io.ReadAll(io.LimitReader(contextFileReader{ctx, file}, (8<<20)+1))
	if err != nil || len(content) > 8<<20 || int64(len(content)) != before.Size() {
		return nil, proof, ErrState
	}
	after, err := file.Stat()
	if err != nil || !sameFile(before, after) {
		return nil, proof, ErrState
	}
	named, err := r.root.Lstat(name)
	if err != nil || !sameFile(before, named) || ctx.Err() != nil || r.check() != nil {
		return nil, proof, ErrState
	}
	identity, err := fileIdentity(before)
	if err != nil {
		return nil, proof, err
	}
	digest := sha256.Sum256(content)
	proof = FileProof{Name: name, Size: before.Size(), Modified: strconv.FormatInt(before.ModTime().UnixNano(), 10), Identity: identity, Digest: hex.EncodeToString(digest[:])}
	return content, proof, nil
}

type contextFileReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Read requires a canonical info hash. A protocol-specific shortened v2 alias
// must first be resolved to its full identity by the caller; it is not guessed.
func (r *MetainfoRoot) Read(ctx context.Context, hash, name string, useResume bool) (Source, error) {
	s := Source{Hash: hash, Root: r.physical, RootIdentity: r.identity}
	if !Hex(hash, 40) && !Hex(hash, 64) {
		return s, ErrState
	}
	if name == "" {
		name = hash + ".torrent"
	}
	if !strings.HasSuffix(name, ".torrent") {
		return s, ErrState
	}
	content, proof, err := r.read(ctx, name)
	if err != nil {
		return s, ErrState
	}
	s.Torrent = proof
	original, err := torrentmeta.Parse(content)
	if err != nil || original.InfoHash != hash {
		return s, ErrState
	}
	prepared, prepareErr := torrentmeta.PrepareTransfer(content, nil)
	var resume []byte
	if useResume && (prepareErr != nil || prepared.TrackerCount == 0) {
		data, proof, err := r.read(ctx, hash+".fastresume")
		if err != nil && !os.IsNotExist(err) {
			return s, ErrState
		}
		if err == nil {
			resume = data
			s.Resume = &proof
		}
	}
	prepared, err = torrentmeta.PrepareTransfer(content, resume)
	if err != nil || prepared.InfoHash != hash {
		return s, ErrState
	}
	if ctx.Err() != nil || r.check() != nil {
		return s, ErrState
	}
	s.Prepared = prepared
	return s, nil
}
func (r *MetainfoRoot) Verify(ctx context.Context, s Source, useResume bool) error {
	if s.Root != r.physical || s.RootIdentity != r.identity {
		return ErrState
	}
	current, err := r.Read(ctx, s.Hash, s.Torrent.Name, useResume)
	if err != nil || current.Torrent != s.Torrent || Hash(current.Resume) != Hash(s.Resume) || current.Prepared.InfoHash != s.Prepared.InfoHash || !equalContent(current.Prepared.Content, s.Prepared.Content) {
		return ErrState
	}
	return nil
}
func equalContent(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
