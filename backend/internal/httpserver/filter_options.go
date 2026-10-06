package httpserver

import (
	"context"

	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
)

func nativeFilterOptions(ctx context.Context, store *filterconfig.Store) (map[string]any, error) {
	groups, err := store.Groups(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]any, 0, len(groups))
	for _, group := range groups {
		values = append(values, map[string]any{"id": group.ID, "name": group.Name})
	}
	return map[string]any{"code": 0, "data": map[string]any{"ruleGroups": values}}, nil
}
