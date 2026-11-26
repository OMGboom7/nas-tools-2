package recognitioncache

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

func ExpireEnabled(snapshot map[string]any) bool {
	lab, _ := snapshot["laboratory"].(map[string]any)
	switch value := lab["tmdb_cache_expire"].(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "y", "yes", "1", "on":
			return true
		}
	case int:
		return value == 1
	}
	return false
}

func Command(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	request, err := ReadRequest(in)
	if err != nil {
		return err
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	expire := false
	if op == "get" || op == "list" {
		snapshot, err := config.NewStore(path).Snapshot()
		if err != nil {
			return err
		}
		expire = ExpireEnabled(snapshot)
	}
	result, err := Run(ctx, (config.Application{Path: path}).UserDatabasePath(), op, request, expire)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
