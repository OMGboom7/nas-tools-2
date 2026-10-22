package remotestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinioMetadataReplacementRequiresExactConditionalVersionAndNeverRetries(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	location := "/bucket/library/movie.nfo"
	key := strings.TrimPrefix(location, "/")
	s.Put(key, []byte("original"))
	previous, err := c.Entry(t.Context(), "/bucket/library", "movie.nfo")
	if err != nil || previous == nil {
		t.Fatal(err)
	}
	bad := *previous
	bad.ID = "changed-version"
	if c.WriteMetadata(t.Context(), location, []byte("must not write"), &bad) == nil {
		t.Fatal("wrong If-Match accepted")
	}
	if data, _ := s.Get(key); string(data) != "original" {
		t.Fatal("conditional failure altered original")
	}
	if err := c.WriteMetadata(t.Context(), location, []byte("replacement"), previous); err != nil {
		t.Fatal(err)
	}
	current, err := c.Entry(t.Context(), "/bucket/library", "movie.nfo")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMetadata(t.Context(), location, nil, current); err != nil {
		t.Fatal("empty original cannot be restored", err)
	}
	current, err = c.Entry(t.Context(), "/bucket/library", "movie.nfo")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.Requests())
	s.FailNextWrite(key, false, 500)
	if err := c.WriteMetadata(t.Context(), location, []byte("stored but response lost"), current); err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal(err)
	}
	writes := 0
	for _, r := range s.Requests()[before:] {
		if r.Method == "PUT" && r.Key == key {
			writes++
			if r.IfMatch == "" {
				t.Fatal("replacement omitted If-Match")
			}
		}
		if r.Method == "DELETE" {
			t.Fatal("metadata writer deleted object")
		}
	}
	if writes != 1 {
		t.Fatal("ambiguous replacement replayed", writes)
	}
	if data, _ := s.Get(key); string(data) != "stored but response lost" {
		t.Fatal("fixture did not store ambiguous write")
	}
	before = len(s.Requests())
	if !errors.Is(c.WriteMetadata(t.Context(), location, make([]byte, (16<<20)+1), current), ErrLimit) || len(s.Requests()) != before {
		t.Fatal("unbounded metadata reached storage")
	}
	if err := c.WriteMetadata(t.Context(), location, []byte("new object without original evidence"), nil); err == nil {
		t.Fatal("absent-only branch replaced an object")
	}
}

func TestRealRcloneMetadataReplacementUsesIsolatedSmallReadonlyInputAndSupportsEmptyOriginal(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit isolated Rclone required")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "library"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "library", "movie;$(never-executed).nfo")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[NASTOOL]\ntype=alias\nremote="+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := (Factory{ConfigFile: conf, Binary: binary, Environment: []string{}}).Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	previous, err := c.Entry(t.Context(), "/library", filepath.Base(file))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMetadata(t.Context(), "/library/"+filepath.Base(file), []byte("replacement"), previous); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if !bytes.Equal(data, []byte("replacement")) {
		t.Fatal("real rcat did not replace exact metadata")
	}
	if err := c.WriteMetadata(t.Context(), "/library/"+filepath.Base(file), nil, previous); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(file)
	if len(data) != 0 {
		t.Fatal("empty original restoration failed")
	}
}
