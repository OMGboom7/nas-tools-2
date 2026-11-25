package torrenttransfer

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// MetadataCommand is local to the two legacy-compatible binaries. Inputs and
// results can contain private trackers and are exchanged only over stdio.
func MetadataCommand(ctx context.Context, op string, in io.Reader, out io.Writer) error {
	limit := int64(8 << 20)
	if op == "resolve" || op == "transfer" {
		limit = 32 << 10
	}
	raw, err := io.ReadAll(io.LimitReader(in, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return ErrState
	}
	switch op {
	case "layout":
		folder, files, err := torrentmeta.DownloadLayout(raw)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Folder string   `json:"folder"`
			Files  []string `json:"files"`
		}{folder, files})
	case "magnet":
		magnet, err := torrentmeta.MagnetLink(raw)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, magnet)
		return err
	case "resolve":
		var q struct {
			Magnet  string
			Seconds int
		}
		if json.Unmarshal(raw, &q) != nil || q.Seconds < 1 || q.Seconds > 45 {
			return ErrState
		}
		call, cancel := context.WithTimeout(ctx, time.Duration(q.Seconds)*time.Second)
		defer cancel()
		raw, err = torrentmeta.ResolveLink(call, q.Magnet)
		if err != nil {
			return err
		}
	case "transfer":
		var q struct {
			Directory, ID, V1, V2 string
			QB                    bool
		}
		if json.Unmarshal(raw, &q) != nil || !Hex(q.ID, 40) && !Hex(q.ID, 64) {
			return ErrState
		}
		hash := q.ID
		if q.V1 != "" || q.V2 != "" {
			if q.V1 != "" && !Hex(q.V1, 40) || q.V2 != "" && !Hex(q.V2, 64) ||
				q.ID != q.V1 && q.ID != q.V2 && (q.V2 == "" || q.ID != q.V2[:40]) {
				return ErrState
			}
			hash = q.V1
			if hash == "" {
				hash = q.V2
			}
		}
		// The result is consumed immediately. Open-file identity checks still apply
		// on systems without serializable device/inode proofs.
		root, err := openMetainfoRoot(q.Directory, false)
		if err != nil {
			return err
		}
		defer root.Close()
		source, err := root.Read(ctx, hash, q.ID+".torrent", q.QB)
		if err != nil || q.V2 != "" && source.Prepared.V2Hash != q.V2 || q.V1 != "" && source.Prepared.V1Hash != q.V1 {
			return ErrState
		}
		raw = source.Prepared.Content
	default:
		return ErrState
	}
	_, err = out.Write(raw)
	return err
}
