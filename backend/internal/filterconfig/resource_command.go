package filterconfig

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

type resourceRequest struct {
	Op     string
	Filter resourceFilter
	Scope  releaseScope
	Group  int64
}

func loadResourceGroups(ctx context.Context, path string) ([]GroupInfo, error) {
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return nil, err
	}
	defer release()
	store, err := Open((config.Application{Path: path}).UserDatabasePath())
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.List(ctx)
}

type resourceSession struct {
	path   string
	groups []GroupInfo
}

func (session *resourceSession) evaluate(ctx context.Context, request resourceRequest) (any, error) {
	if request.Op == "scope" {
		return request.Scope.matches(), nil
	}
	group := request.Group
	if request.Op == "match" {
		group = request.Filter.Group
	}
	if session.groups == nil && group != -1 {
		groups, err := loadResourceGroups(ctx, session.path)
		if err != nil {
			return nil, err
		}
		session.groups = groups
	}
	switch request.Op {
	case "match":
		return request.Filter.evaluate(ctx, session.groups)
	case "highest", "needsPromotion":
		selected, err := selectResourceGroup(session.groups, group)
		if err != nil {
			return nil, err
		}
		if request.Op == "needsPromotion" {
			if selected != nil {
				for _, rule := range selected.Rules {
					if rule.Free != "" {
						return true, nil
					}
				}
			}
			return false, nil
		}
		if selected == nil || len(selected.Rules) == 0 {
			return 100, nil
		}
		value := strings.TrimSpace(selected.Rules[0].Priority)
		if value == "" {
			return 100, nil
		}
		priority, err := strconv.Atoi(value)
		return 100 - priority, err
	default:
		return nil, errors.New("unknown filter operation")
	}
}

// ServeResources keeps one rule snapshot for all candidates from one site search.
// The database and the startup lock are released before writing the first decision.
func ServeResources(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	session := resourceSession{path: path}
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 4096), 512<<10)
	encoder := json.NewEncoder(out)
	for lines.Scan() {
		var request resourceRequest
		decoder := json.NewDecoder(bytes.NewReader(lines.Bytes()))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			return errors.New("invalid resource filter request")
		}
		evalCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := session.evaluate(evalCtx, request)
		cancel()
		if err != nil {
			result = map[string]bool{"error": true}
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return lines.Err()
}
