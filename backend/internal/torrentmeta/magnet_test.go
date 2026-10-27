package torrentmeta

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var metadataHelperRaw = manifestMetainfo(map[string]any{"length": 12, "name": "Show.S01E02.mkv"})

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--no-conf=true" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		dir := ""
		required := map[string]bool{"--bt-metadata-only=true": false, "--bt-save-metadata=true": false, "--follow-torrent=false": false, "--enable-rpc=false": false}
		for _, arg := range os.Args[1:] {
			if strings.Contains(arg, "magnet:") || strings.Contains(arg, "private-tracker") {
				os.Exit(21)
			}
			if strings.HasPrefix(arg, "--dir=") {
				dir = strings.TrimPrefix(arg, "--dir=")
			}
			if _, ok := required[arg]; ok {
				required[arg] = true
			}
		}
		for _, ok := range required {
			if !ok {
				os.Exit(22)
			}
		}
		if os.Getenv("NASTOOL_METADATA_AMBIENT_SECRET") != "" || dir == "" || !strings.Contains(line, "private-tracker") {
			os.Exit(23)
		}
		meta, err := Parse(metadataHelperRaw)
		if err != nil {
			os.Exit(24)
		}
		if os.WriteFile(filepath.Join(dir, meta.InfoHash+".torrent"), metadataHelperRaw, 0600) != nil {
			os.Exit(25)
		}
		fmt.Fprint(os.Stdout, "private-tracker output must not escape")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestMetadataOnlyCommandUsesStdinNoAmbientCredentialsAndVerifiesHash(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NASTOOL_ARIA2_BINARY", binary)
	t.Setenv("NASTOOL_METADATA_AMBIENT_SECRET", "never-forward")
	meta, err := Parse(metadataHelperRaw)
	if err != nil {
		t.Fatal(err)
	}
	magnet := "magnet:?xt=urn:btih:" + meta.InfoHash + "&tr=http%3A%2F%2Fprivate-tracker"
	raw, err := ResolveMagnet(t.Context(), magnet, meta.InfoHash)
	if err != nil || string(raw) != string(metadataHelperRaw) {
		t.Fatal(string(raw), err)
	}
	if _, err := ResolveMagnet(t.Context(), strings.Replace(magnet, meta.InfoHash, strings.Repeat("a", 40), 1), strings.Repeat("a", 40)); err == nil {
		t.Fatal("wrong metadata hash accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ResolveMagnet(ctx, magnet, meta.InfoHash); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestMetadataResolverUnavailableNeverFallsBackToPythonOrWholeMagnet(t *testing.T) {
	t.Setenv("NASTOOL_ARIA2_BINARY", filepath.Join(t.TempDir(), "missing"))
	if _, err := ResolveMagnet(t.Context(), "magnet:?xt=urn:btih:"+strings.Repeat("a", 40), strings.Repeat("a", 40)); err == nil {
		t.Fatal("missing resolver accepted")
	}
	if _, err := ResolveMagnet(t.Context(), "magnet:?bad\n--seed=true", strings.Repeat("a", 40)); err == nil {
		t.Fatal("line injection")
	}
}
