package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
)

type backupRestoreAPI struct {
	login *siteLoginAPI
	path  string
}

func (api backupRestoreAPI) serve(w http.ResponseWriter, r *http.Request) {
	if api.path == "" {
		writeAPIError(w, 503, 503, "备份恢复不可用")
		return
	}
	if _, ok := api.login.actor(w, r, false); !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	workspace, err := backuprestore.New(api.path)
	if err != nil {
		writeAPIError(w, 503, 503, "恢复目录不可用")
		return
	}
	if r.Method == http.MethodGet {
		receipt, err := workspace.Receipt(r.PathValue("id"))
		if err != nil {
			writeAPIError(w, 404, 404, "未找到原恢复记录")
			return
		}
		signinJSON(w, receipt)
		return
	}
	if !api.login.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式校验和暂存恢复包")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backuprestore.MaxArchive+64<<10)
	var input io.Reader = r.Body
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch kind {
	case "application/zip", "application/octet-stream":
	case "multipart/form-data":
		form, err := r.MultipartReader()
		if err != nil {
			writeAPIError(w, 400, 400, "无效的归档上传")
			return
		}
		file, err := form.NextPart()
		if err != nil || file.FormName() != "file" {
			writeAPIError(w, 400, 400, "请上传 ZIP 文件")
			return
		}
		input = file
	default:
		writeAPIError(w, 415, 415, "请上传 ZIP 文件。恢复只在服务停止后通过 Go 命令应用")
		return
	}
	stage, err := workspace.Stage(r.Context(), input)
	if err != nil {
		var size *http.MaxBytesError
		switch {
		case errors.As(err, &size):
			writeAPIError(w, 413, 413, "备份归档过大")
		case errors.Is(err, backuprestore.ErrArchive):
			writeAPIError(w, 400, 400, "归档未通过 ZIP、配置或 SQLite 校验")
		case errors.Is(err, backuprestore.ErrBusy):
			writeAPIError(w, 409, 409, "已有离线恢复操作正在执行")
		case errors.Is(err, os.ErrPermission):
			writeAPIError(w, 503, 503, "无法写入私有恢复目录")
		default:
			writeAPIError(w, 400, 400, "无法校验或暂存备份文件")
		}
		return
	}
	signinJSON(w, stage)
}

func (api backupRestoreAPI) legacy(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		var envelope struct {
			Cmd string `json:"cmd"`
		}
		if err != nil || json.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "原操作格式无效")
			return
		}
		if strings.EqualFold(strings.TrimSpace(envelope.Cmd), "restory_backup") {
			writeAPIError(w, 410, 410, "旧文件名恢复入口已迁移，请在服务页上传 ZIP 并按提示离线恢复")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}
