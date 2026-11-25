package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type pluginEditorForm struct {
	ID               string
	Level            *int
	Digest, Revision string
	Detail           map[string]any
}

type pluginEditorRow struct {
	ID     string         `json:"id"`
	Detail map[string]any `json:"detail"`
}

// PluginEditors joins live extension forms with the installed IDs and settings
// from one database snapshot, preserving the loader's display order.
func PluginEditors(ctx context.Context, path, access string, in io.Reader, out io.Writer) error {
	level, err := strconv.Atoi(access)
	if err != nil || (level != 1 && level != 2) {
		return errors.New("invalid editor access")
	}
	var request struct {
		RevisionsOnly bool
		Selected      *string
		Forms         []pluginEditorForm
	}
	input := &io.LimitedReader{R: in, N: 32 << 20}
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 || request.Forms == nil || len(request.Forms) > 1024 {
		return errors.New("invalid plugin forms")
	}
	if request.RevisionsOnly && level != 2 {
		return errors.New("administrator required for external editor revisions")
	}
	record := false
	for _, form := range request.Forms {
		if form.Digest != "" && level == 2 && !request.RevisionsOnly {
			record = true
			break
		}
	}
	db, closeDB, err := openPluginDatabase(path, !record)
	if err != nil {
		return err
	}
	defer closeDB()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !record})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	installed, err := readPluginInstallation(ctx, tx)
	if err != nil {
		return err
	}
	rows := make([]pluginEditorRow, 0, len(request.Forms))
	revisions := map[string]string{}
	for _, form := range request.Forms {
		if form.Level == nil || *form.Level > level || *form.Level < 0 || request.Selected != nil && form.ID != *request.Selected || !slices.Contains(installed, form.ID) {
			continue
		}
		if _, duplicate := revisions[form.ID]; duplicate || (!request.RevisionsOnly && form.Detail == nil) {
			return errors.New("invalid installed plugin form")
		}
		value, raw, err := readPluginValue(ctx, tx, "plugin."+form.ID)
		if err != nil {
			return err
		}
		revision := pluginConfigRevision(raw)
		revisions[form.ID] = revision
		if request.RevisionsOnly {
			continue
		}
		// Historically an empty list became {} in the aggregate view but
		// retained its stored type when the editor requested a particular ID.
		trimmed := strings.TrimRight(raw, " \r\n\t")
		if raw == "" || request.Selected == nil && strings.HasPrefix(raw, "[") && strings.TrimSpace(trimmed[1:len(trimmed)-1]) == "" {
			value = map[string]any{}
		}
		form.Detail["config"] = value
		delete(form.Detail, "config_revision")
		if request.Selected != nil {
			form.Detail["config_revision"] = revision
		}
		if level == 2 && form.Digest != "" {
			if revision != form.Revision {
				return ErrPluginConfigChanged
			}
			if err := recordEditorFields(ctx, tx, form); err != nil {
				return err
			}
		}
		rows = append(rows, pluginEditorRow{form.ID, form.Detail})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if request.RevisionsOnly {
		return json.NewEncoder(out).Encode(revisions)
	}
	return json.NewEncoder(out).Encode(rows)
}

func recordEditorFields(ctx context.Context, tx *sql.Tx, form pluginEditorForm) error {
	digest, err := hex.DecodeString(form.Digest)
	if err != nil || len(digest) != 32 {
		return errors.New("invalid extension digest")
	}
	keys := map[string]bool{}
	var visit func(any)
	visit = func(node any) {
		if len(keys) > 300 {
			return
		}
		switch v := node.(type) {
		case []any:
			for _, item := range v {
				visit(item)
			}
		case map[string]any:
			if key, ok := v["id"].(string); ok && key != "" && utf8.RuneCountInString(key) <= 120 {
				keys[key] = true
			}
			visit(v["content"])
		}
	}
	visit(form.Detail["fields"])
	if len(keys) > 300 {
		return nil
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	slices.Sort(ordered)
	data, err := json.Marshal(map[string]any{"digest": form.Digest, "revision": form.Revision, "keys": ordered})
	if err != nil {
		return err
	}
	key := "GoExternalPluginFields." + form.ID
	return systemconfig.UpdateManyTx(ctx, tx, []string{key}, func(current map[string]string) (map[string]string, error) {
		if current[key] == string(data) {
			return nil, nil
		}
		return map[string]string{key: string(data)}, nil
	})
}
