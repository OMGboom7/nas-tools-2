package transmission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
	_ "modernc.org/sqlite"
)

func TestTransferInterruptedAfterDownloaderAcceptsAdd(t *testing.T) {
	const workerMode = "NASTOOL_TEST_TRANSFER_PROCESS"
	meta, err := torrentmeta.PrepareTransfer(transferMetainfo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mode := os.Getenv(workerMode); mode != "" {
		runInterruptedTransferClient(t, mode, meta)
		return
	}

	accepted := make(chan struct{})
	var adds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method    string
			Arguments struct {
				Metainfo  []byte
				Directory string `json:"download-dir"`
				Paused    bool
			}
		}
		if r.URL.Path != "/transmission/rpc" || r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&rpc) != nil {
			http.Error(w, "invalid RPC", http.StatusBadRequest)
			return
		}
		switch rpc.Method {
		case "session-get":
			io.WriteString(w, `{"tag":1,"result":"success","arguments":{"version":"4.0.6","rpc-version":17}}`)
		case "torrent-add":
			count := adds.Add(1)
			if !bytes.Equal(rpc.Arguments.Metainfo, meta.Content) || !rpc.Arguments.Paused || rpc.Arguments.Directory != "/library" {
				http.Error(w, "unexpected transfer", http.StatusBadRequest)
				return
			}
			if count == 1 {
				close(accepted)
				// Keep the reply in flight until the sender dies.
				<-r.Context().Done()
			}
		default:
			http.Error(w, "unexpected RPC", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "user.db")
	command := func(mode string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTransferInterruptedAfterDownloaderAcceptsAdd$")
		cmd.Env = append(os.Environ(), workerMode+"="+mode, "NASTOOL_TEST_TRANSFER_URL="+server.URL, "NASTOOL_TEST_TRANSFER_DB="+dbPath)
		return cmd
	}
	first := command("submit")
	var output bytes.Buffer
	first.Stdout, first.Stderr = &output, &output
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	select {
	case <-accepted:
		if err := first.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		var interrupted *exec.ExitError
		if err := <-done; !errors.As(err, &interrupted) {
			t.Fatalf("sender was not interrupted: %v", err)
		}
	case err := <-done:
		t.Fatalf("sender exited before acceptance: %v\n%s", err, output.Bytes())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	if out, err := command("recover").CombinedOutput(); err != nil {
		t.Fatalf("recovery: %v\n%s", err, out)
	}
	if count := adds.Load(); count != 1 {
		t.Fatalf("downloader received %d add requests, want 1", count)
	}
}

func runInterruptedTransferClient(t *testing.T, mode string, meta torrentmeta.TransferMetadata) {
	t.Helper()
	db, err := sql.Open("sqlite", os.Getenv("NASTOOL_TEST_TRANSFER_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	ledger, err := torrenttransfer.NewLedger(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(os.Getenv("NASTOOL_TEST_TRANSFER_URL"), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := torrenttransfer.Hash("original receipt")
	intent := torrenttransfer.AddIntent{
		Ledger: ledger, Receipt: original, ScopeProof: torrenttransfer.Hash("fixture scope"),
		Receiver: torrenttransfer.Hash(c.Address()), WireID: meta.V1Hash, Directory: "/library",
		ContentDigest: fmt.Sprintf("%x", sha256.Sum256(meta.Content)),
		Revalidate:    func(context.Context) error { return nil },
	}
	if mode == "submit" {
		_, err := c.AddReviewedTransfer(ctx, transferMetainfo, intent.Directory, intent)
		t.Fatalf("submission returned before interruption: %v", err)
	}
	if mode != "recover" {
		t.Fatal("invalid worker mode")
	}
	key := torrenttransfer.ActionKey{Receiver: intent.Receiver, WireID: intent.WireID, Action: "add"}
	for _, receipt := range []string{original, torrenttransfer.Hash("later cycle")} {
		intent.Receipt = receipt
		if _, err := c.AddReviewedTransfer(ctx, transferMetainfo, intent.Directory, intent); !errors.Is(err, torrenttransfer.ErrPriorAdd) {
			t.Fatalf("retry of %s: %v", receipt, err)
		}
		if _, err := ledger.AddReply(ctx, key, receipt); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unexpected stored reply for %s: %v", receipt, err)
		}
	}
	action, err := ledger.Lookup(ctx, key)
	proof := torrenttransfer.Hash([]string{intent.ScopeProof, intent.Directory, intent.ContentDigest})
	if err != nil || action.State != "pending" || action.Receipt != original || action.Proof != proof {
		t.Fatalf("original intent after restart: %+v, %v", action, err)
	}
}
