package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/httpserver"
)

func restoreCommand(ctx context.Context, args []string, configPath string, out io.Writer) error {
	if len(args) == 0 {
		return restoreUsage()
	}
	workspace, err := backuprestore.New(configPath)
	if err != nil {
		return err
	}
	var result any
	switch args[0] {
	case "stage":
		if len(args) != 2 {
			return restoreUsage()
		}
		input, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > backuprestore.MaxArchive {
			return backuprestore.ErrArchive
		}
		result, err = workspace.Stage(ctx, input)
		if err != nil {
			return err
		}
	case "apply", "rollback":
		if len(args) != 3 || args[2] != "--offline" {
			return restoreUsage()
		}
		if args[0] == "apply" {
			result, err = workspace.Apply(ctx, args[1], true)
		} else {
			result, err = workspace.Rollback(ctx, args[1], true)
		}
	case "resume":
		if len(args) != 3 || args[2] != "--confirm-resume" {
			return restoreUsage()
		}
		result, err = workspace.Resume(ctx, args[1], true, func(path string) error {
			_, err := httpserver.New(config.Config{
				DisableLegacy: true, ApplicationConfigPath: path, BrowserBinary: "disabled",
			})
			if err != nil {
				return fmt.Errorf("恢复副本无法通过 Go 启动检查，仍可回滚: %w", err)
			}
			return nil
		})
	case "status":
		if len(args) != 1 {
			return restoreUsage()
		}
		result, err = workspace.Status()
		if errors.Is(err, os.ErrNotExist) {
			result, err = map[string]string{"phase": "idle"}, nil
		}
	default:
		return restoreUsage()
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func restoreUsage() error {
	return fmt.Errorf("用法: nas-tools-go restore stage ZIP | apply ID --offline | rollback ID --offline | resume ID --confirm-resume | status")
}
