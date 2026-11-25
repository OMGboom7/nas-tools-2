package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autosub"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func autoSubTables(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_AUTOSUB_RUNS(id TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS GO_AUTOSUB_FILES(job TEXT NOT NULL,seq INTEGER NOT NULL,root INTEGER NOT NULL,path TEXT NOT NULL,media TEXT NOT NULL,state TEXT NOT NULL,note TEXT NOT NULL,PRIMARY KEY(job,seq));
 CREATE INDEX IF NOT EXISTS GO_AUTOSUB_NEXT ON GO_AUTOSUB_FILES(job,state,seq);
 CREATE TABLE IF NOT EXISTS GO_AUTOSUB_OUTPUT(job TEXT NOT NULL,seq INTEGER NOT NULL,target TEXT NOT NULL,proof TEXT NOT NULL,PRIMARY KEY(job,seq,target));
 CREATE TABLE IF NOT EXISTS GO_AUTOSUB_TRANSLATION(job TEXT NOT NULL,seq INTEGER NOT NULL,input TEXT NOT NULL,reply TEXT NOT NULL,PRIMARY KEY(job,seq,input))`)
	return err
}

type subtitleFile struct {
	seq, root                int
	path, media, state, note string
}

func (a *autoSubAPI) inventory(ctx context.Context, s diskRun, c autoSubSettings, current func() error) error {
	var exists bool
	err := a.store.Transaction(ctx, func(tx *sql.Tx) error {
		if e := autoSubTables(ctx, tx); e != nil {
			return e
		}
		return tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM GO_AUTOSUB_RUNS WHERE id=?)", s.ID).Scan(&exists)
	})
	if err != nil || exists {
		return err
	}
	minimum, err := strconv.ParseInt(c.Minimum, 10, 64)
	if err != nil {
		return err
	}
	var files []subtitleFile
	seen := map[string]bool{}
	for _, p := range s.Unavailable {
		files = append(files, subtitleFile{path: p, state: "failed", note: "目录不可访问"})
	}
	for i, root := range s.Roots {
		if err := current(); err != nil {
			return err
		}
		actual, e := organization.FreezeDiskRoot(root.Configured)
		if e != nil || actual != root {
			files = append(files, subtitleFile{path: root.Configured, state: "failed", note: "目录已变化"})
			continue
		}
		scan, e := organization.ScanLibrary(ctx, organization.AutoSubtitleLibrary(root))
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			files = append(files, subtitleFile{path: root.Configured, state: "failed", note: "目录扫描失败"})
			continue
		}
		for _, m := range scan.Files {
			path := filepath.Join(root.Path, m.Path)
			if seen[path] {
				continue
			}
			seen[path] = true
			item := subtitleFile{root: i, path: path, state: "pending", note: "待处理"}
			if m.Blocked != "" {
				item.state, item.note = "skipped", "媒体文件不可读取"
			} else if m.Input.Size < minimum {
				continue
			}
			b, e := json.Marshal(m)
			if e != nil {
				return e
			}
			item.media = string(b)
			files = append(files, item)
		}
	}
	if err := current(); err != nil {
		return err
	}
	return a.store.Transaction(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "INSERT INTO GO_AUTOSUB_RUNS VALUES(?)", s.ID); e != nil {
			return e
		}
		stmt, e := tx.PrepareContext(ctx, "INSERT INTO GO_AUTOSUB_FILES VALUES(?,?,?,?,?,?,?)")
		if e != nil {
			return e
		}
		defer stmt.Close()
		for i, f := range files {
			if _, e = stmt.ExecContext(ctx, s.ID, i, f.root, f.path, f.media, f.state, f.note); e != nil {
				return e
			}
		}
		return nil
	})
}

func (a *autoSubAPI) execute(ctx context.Context, s diskRun, c autoSubSettings, config map[string]any, current func() error) (string, error) {
	if err := a.inventory(ctx, s, c, current); err != nil {
		return "", err
	}
	for {
		if err := current(); err != nil {
			return "", err
		}
		var f subtitleFile
		err := a.store.Transaction(ctx, func(tx *sql.Tx) error {
			e := tx.QueryRowContext(ctx, "SELECT seq,root,path,media,state FROM GO_AUTOSUB_FILES WHERE job=? AND state IN ('pending','running') ORDER BY seq LIMIT 1", s.ID).Scan(&f.seq, &f.root, &f.path, &f.media, &f.state)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, "UPDATE GO_AUTOSUB_FILES SET state='running',note='处理中' WHERE job=? AND seq=?", s.ID, f.seq)
			return e
		})
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return "", err
		}
		var m organization.LibraryMedia
		if f.root < 0 || f.root >= len(s.Roots) || json.Unmarshal([]byte(f.media), &m) != nil {
			return "", organization.ErrState
		}
		work, cancel := context.WithTimeout(ctx, 3*time.Hour)
		msg, err := a.processFile(work, s, f, m, c, config, current)
		cancel()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if e := current(); e != nil {
			return "", e
		}
		state := "done"
		if err != nil {
			state = "failed"
			msg = "处理失败：" + err.Error()
		}
		err = a.store.Transaction(ctx, func(tx *sql.Tx) error {
			if _, e := tx.ExecContext(ctx, "UPDATE GO_AUTOSUB_FILES SET state=?,note=? WHERE job=? AND seq=?", state, msg, s.ID, f.seq); e != nil {
				return e
			}
			if c.Notify {
				key := sitelogin.Hash([]any{autoSubID, s.ID, f.seq})
				return bus.EmitData(ctx, tx, "plugin_message", key, key, "自动字幕生成", filepath.Base(f.path)+"："+msg, map[string]string{"plugin": autoSubID})
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	var total, failed int
	err := a.store.Transaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT count(*),count(CASE WHEN state='failed' THEN 1 END) FROM GO_AUTOSUB_FILES WHERE job=?", s.ID).Scan(&total, &failed)
	})
	return fmt.Sprintf("已检查 %d 项，失败 %d 项。", total, failed), err
}
func (a *autoSubAPI) processFile(ctx context.Context, s diskRun, f subtitleFile, m organization.LibraryMedia, c autoSubSettings, config map[string]any, current func() error) (string, error) {
	root := s.Roots[f.root]
	d := organization.AutoSubtitleLibrary(root)
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := organization.FreezeDiskRoot(root.Configured)
		if err != nil || actual != root {
			return organization.ErrPath
		}
		if err := organization.VerifyLibraryMedia(ctx, d, m); err != nil {
			return err
		}
		return current()
	}
	if err := guard(); err != nil {
		return "", err
	}
	if f.state == "running" {
		var proofs []organization.LibraryOutputProof
		err := a.store.Transaction(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, "SELECT proof FROM GO_AUTOSUB_OUTPUT WHERE job=? AND seq=?", s.ID, f.seq)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var b string
				var p organization.LibraryOutputProof
				if err := rows.Scan(&b); err != nil {
					return err
				}
				if err := json.Unmarshal([]byte(b), &p); err != nil {
					return err
				}
				proofs = append(proofs, p)
			}
			return rows.Err()
		})
		if err != nil {
			return "", err
		}
		completed := false
		for _, p := range proofs {
			state, err := organization.ObserveLibrarySidecar(ctx, d, p)
			if err != nil && organization.AutoSubtitleStageGone(ctx, d, m, p) {
				if e := guard(); e != nil {
					return "", e
				}
				e := a.store.Transaction(ctx, func(tx *sql.Tx) error {
					_, e := tx.ExecContext(ctx, "DELETE FROM GO_AUTOSUB_OUTPUT WHERE job=? AND seq=? AND target=?", s.ID, f.seq, p.Target)
					return e
				})
				if e != nil {
					return "", e
				}
				continue
			}
			if err != nil || state != "created" {
				return "", errors.New("中断后的字幕文件需核对：" + p.Target)
			}
			if !c.Translate || strings.HasSuffix(p.Target, ".zh.srt") {
				completed = true
			}
		}
		if completed {
			return "已核对中断前生成的字幕", nil
		}
	}
	publish := autosub.Publication{Current: guard, Prepare: func(p organization.LibraryOutputProof) error {
		if e := guard(); e != nil {
			return e
		}
		b, e := json.Marshal(p)
		if e != nil {
			return e
		}
		return a.store.Transaction(ctx, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, "INSERT INTO GO_AUTOSUB_OUTPUT VALUES(?,?,?,?)", s.ID, f.seq, p.Target, string(b))
			return e
		})
	}}
	translate := func(ctx context.Context, input string) (string, error) {
		return a.translate(ctx, s.ID, f.seq, config, input, guard)
	}
	return autosub.Process(ctx, d, m, c.Options, publish, translate)
}

var errTranslationUnknown = errors.New("翻译请求结果待核对")

func (a *autoSubAPI) translate(ctx context.Context, id string, seq int, config map[string]any, input string, guard func() error) (string, error) {
	ai, err := sitesignin.QuestionAI(config)
	if err != nil || ai.Key == "" {
		return "", errors.New("翻译服务未配置")
	}
	key := sitelogin.Hash(input)
	var cached string
	if err := guard(); err != nil {
		return "", err
	}
	err = a.store.Transaction(ctx, func(tx *sql.Tx) error {
		e := tx.QueryRowContext(ctx, "SELECT reply FROM GO_AUTOSUB_TRANSLATION WHERE job=? AND seq=? AND input=?", id, seq, key).Scan(&cached)
		if errors.Is(e, sql.ErrNoRows) {
			_, e = tx.ExecContext(ctx, "INSERT INTO GO_AUTOSUB_TRANSLATION VALUES(?,?,?,'')", id, seq, key)
			return e
		}
		if e == nil && cached == "" {
			return errTranslationUnknown
		}
		return e
	})
	if err != nil || cached != "" {
		return cached, err
	}
	client := &http.Client{Transport: signinAuxTransport{base: a.transport, configuration: config}, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	payload, err := json.Marshal(map[string]any{"model": ai.Model, "user": "NAStool", "temperature": 0, "top_p": 1, "frequency_penalty": 0, "presence_penalty": 0, "messages": []map[string]string{{"role": "system", "content": "You are a translation engine that can only translate text and cannot interpret it."}, {"role": "user", "content": "translate to zh-CN:\n\n" + input}}})
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < 4; attempt++ {
		if e := guard(); e != nil {
			return "", e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, ai.URL, bytes.NewReader(payload))
		if e != nil {
			return "", sitesignin.ErrScope
		}
		req.Header.Set("Authorization", "Bearer "+ai.Key)
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			return "", errTranslationUnknown
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
		res.Body.Close()
		if res.StatusCode == 429 && attempt < 3 {
			delay := time.Duration(attempt+1) * time.Second
			if value := res.Header.Get("Retry-After"); value != "" {
				n, err := strconv.Atoi(value)
				if err == nil && n >= 0 && n <= 60 {
					delay = time.Duration(n) * time.Second
				} else {
					return "", errors.New("翻译服务限流，请稍后再运行")
				}
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if e != nil || len(b) > 1<<20 {
			return "", errTranslationUnknown
		}
		if res.StatusCode != 200 {
			return "", fmt.Errorf("翻译服务返回 HTTP %d", res.StatusCode)
		}
		var out struct {
			Choices []struct{ Message struct{ Content string } }
		}
		if json.Unmarshal(b, &out) != nil || len(out.Choices) == 0 {
			return "", errTranslationUnknown
		}
		reply := strings.TrimSpace(out.Choices[0].Message.Content)
		if reply == "" {
			return "", errTranslationUnknown
		}
		err = a.store.Transaction(ctx, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, "UPDATE GO_AUTOSUB_TRANSLATION SET reply=? WHERE job=? AND seq=? AND input=? AND reply=''", reply, id, seq, key)
			return e
		})
		return reply, err
	}
	return "", errTranslationUnknown
}
