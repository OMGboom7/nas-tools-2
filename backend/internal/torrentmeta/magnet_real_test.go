package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A real CLI speaks BEP 9 to one loopback peer. There is no public tracker,
// DHT, media file or payload block, and any data-block request fails the test.
func TestRealAria2MetadataOnlyNeverRequestsMediaPayload(t *testing.T) {
	executable := os.Getenv("NASTOOL_TEST_ARIA2_BINARY")
	if executable == "" {
		t.Skip("explicit real aria2 binary not configured")
	}
	t.Setenv("NASTOOL_ARIA2_BINARY", executable)
	t.Setenv("NASTOOL_METADATA_DISABLE_DHT", "true")
	info := []byte(manifestEncode(map[string]any{"length": 16384, "name": "Show.S01E02.mkv", "piece length": 16384, "pieces": strings.Repeat("x", 20)}))
	digest := sha1.Sum(info)
	hash := hex.EncodeToString(digest[:])
	peer, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var payloadRequests atomic.Int32
	var metadataRequests atomic.Int32
	go func() {
		for {
			conn, err := peer.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				handshake := make([]byte, 68)
				if _, err := io.ReadFull(conn, handshake); err != nil || !bytes.Equal(handshake[28:48], digest[:]) {
					return
				}
				reply := append([]byte{}, handshake...)
				reply[25] |= 0x10
				copy(reply[48:], []byte("-NT0001-local-peer---"))
				if _, err := conn.Write(reply); err != nil {
					return
				}
				send := func(id byte, body []byte) bool {
					msg := append([]byte{20, id}, body...)
					header := make([]byte, 4)
					binary.BigEndian.PutUint32(header, uint32(len(msg)))
					_, err := conn.Write(append(header, msg...))
					return err == nil
				}
				if !send(0, []byte(manifestEncode(map[string]any{"m": map[string]any{"ut_metadata": 1}, "metadata_size": len(info)}))) {
					return
				}
				clientExtension := byte(0)
				for {
					header := make([]byte, 4)
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					n := binary.BigEndian.Uint32(header)
					if n == 0 {
						continue
					}
					if n > 1<<20 {
						return
					}
					body := make([]byte, int(n))
					if _, err := io.ReadFull(conn, body); err != nil {
						return
					}
					if body[0] == 6 {
						payloadRequests.Add(1)
						return
					}
					if len(body) < 2 || body[0] != 20 {
						continue
					}
					if body[1] == 0 {
						pos, nodes := 0, 0
						v, ok := manifestNode(body[2:], &pos, 0, &nodes)
						if ok {
							m, ok := v.(map[string]any)
							if ok {
								extensions, ok := m["m"].(map[string]any)
								if ok {
									value, _ := extensions["ut_metadata"].(int64)
									if value > 0 && value < 256 {
										clientExtension = byte(value)
									}
								}
							}
						}
						continue
					}
					if body[1] == 1 && clientExtension > 0 {
						metadataRequests.Add(1)
						prefix := []byte(manifestEncode(map[string]any{"msg_type": 1, "piece": 0, "total_size": len(info)}))
						if !send(clientExtension, append(prefix, info...)) {
							return
						}
					}
				}
			}()
		}
	}()
	port := peer.Addr().(*net.TCPAddr).Port
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("info_hash") != string(digest[:]) {
			w.WriteHeader(400)
			return
		}
		compact := []byte{127, 0, 0, 1, byte(port >> 8), byte(port)}
		io.WriteString(w, manifestEncode(map[string]any{"interval": 60, "complete": 1, "incomplete": 0, "peers": string(compact)}))
	}))
	defer tracker.Close()
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Show.S01E02.mkv&tr=" + url.QueryEscape(tracker.URL+"/announce")
	raw, err := ResolveMagnet(t.Context(), magnet, hash)
	if err != nil {
		t.Fatal("real metadata-only exchange failed", err, "metadata requests", metadataRequests.Load(), "peer port", strconv.Itoa(port))
	}
	actual, files, err := Manifest(raw)
	if err != nil || actual.InfoHash != hash || len(files) != 1 || metadataRequests.Load() == 0 || payloadRequests.Load() != 0 {
		t.Fatal(actual, files, err, metadataRequests.Load(), payloadRequests.Load())
	}
}
