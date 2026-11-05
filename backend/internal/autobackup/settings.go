package autobackup

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Plugin    = "AutoBackup"
	ConfigKey = "plugin." + Plugin
)

var (
	ErrSettings = errors.New("备份设置无效")
	ErrScope    = errors.New("备份配置、目录或授权已变化")
	ErrBusy     = errors.New("备份正在执行")
	ErrArchive  = errors.New("备份文件无法核验")
)

type Settings struct {
	Enabled  bool   `json:"enabled"`
	Full     bool   `json:"full"`
	Notify   bool   `json:"notify"`
	OnlyOnce bool   `json:"onlyonce"`
	Cron     string `json:"cron"`
	Keep     string `json:"cnt"`
	Path     string `json:"bk_path"`
}

func Parse(raw string) (Settings, error) {
	fields, err := decode(raw)
	var out Settings
	if err != nil {
		return out, err
	}
	for key, target := range map[string]*bool{"enabled": &out.Enabled, "full": &out.Full, "notify": &out.Notify, "onlyonce": &out.OnlyOnce} {
		if field, ok := fields[key]; ok && json.Unmarshal(field, target) != nil {
			return Settings{}, ErrSettings
		}
	}
	for key, target := range map[string]*string{"cron": &out.Cron, "cnt": &out.Keep, "bk_path": &out.Path} {
		if field, ok := fields[key]; ok && json.Unmarshal(field, target) != nil {
			if key != "cnt" {
				return Settings{}, ErrSettings
			}
			var number int
			if json.Unmarshal(field, &number) != nil {
				return Settings{}, ErrSettings
			}
			*target = strconv.Itoa(number)
		}
		*target = strings.TrimSpace(*target)
	}
	if out.Keep != "" {
		n, err := strconv.Atoi(out.Keep)
		if err != nil || n < 0 || n > 10000 {
			return Settings{}, ErrSettings
		}
	}
	if len(out.Path) > 4096 || strings.ContainsRune(out.Path, 0) {
		return Settings{}, ErrSettings
	}
	return out, nil
}

func (s Settings) Limit() int {
	n, _ := strconv.Atoi(s.Keep)
	return n
}

func (s Settings) Destination(configPath string, container bool) (string, error) {
	if s.Path == "" || container {
		return filepath.Abs(filepath.Join(filepath.Dir(configPath), "backup_file"))
	}
	return filepath.Abs(s.Path)
}

func decode(raw string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if raw != "" && (len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &out) != nil || out == nil) {
		return nil, ErrSettings
	}
	return out, nil
}

func Merge(raw string, changes map[string]json.RawMessage) (string, error) {
	out, err := decode(raw)
	if err != nil {
		return "", err
	}
	for key, value := range changes {
		switch key {
		case "enabled", "full", "notify", "onlyonce", "cron", "cnt", "bk_path":
			out[key] = value
		default:
			return "", ErrSettings
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	_, err = Parse(string(data))
	return string(data), err
}
