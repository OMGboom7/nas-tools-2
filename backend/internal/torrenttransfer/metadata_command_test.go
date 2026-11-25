package torrenttransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetadataCommandNeedsFullV2IdentityForShortBackupName(t *testing.T) {
	dir := t.TempDir()
	info := []byte("d9:file treed1:ad0:d6:lengthi1eeee12:meta versioni2e4:name1:ae")
	sum := sha256.Sum256(info)
	full := hex.EncodeToString(sum[:])
	short := full[:40]
	data := append(append([]byte("d4:info"), info...), 'e')
	if err := os.WriteFile(filepath.Join(dir, short+".torrent"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, v2 := range []string{full, "", short + strings.Repeat("0", 24), "0"} {
		input, _ := json.Marshal(map[string]any{"directory": dir, "id": short, "v2": v2, "qb": true})
		var output bytes.Buffer
		err := MetadataCommand(t.Context(), "transfer", bytes.NewReader(input), &output)
		if v2 == full {
			if err != nil || !bytes.Equal(output.Bytes(), data) {
				t.Fatal(err)
			}
		} else if err == nil || output.Len() != 0 {
			t.Fatal("partial or foreign identity accepted")
		}
	}
}
