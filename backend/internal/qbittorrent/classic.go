package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type ClassicDirectory struct {
	Label    string
	SavePath string `json:"save_path"`
}

// Legacy automatic management uses the configured category path. Limit
// category creation/updates to the category actually used by this submission.
func (c *Client) classicManagement(ctx context.Context, form url.Values, mode string, dirs []ClassicDirectory) error {
	raw, err := c.request(ctx, http.MethodGet, "app/preferences", nil)
	if err != nil {
		return err
	}
	var prefs struct {
		Auto       bool `json:"auto_tmm_enabled"`
		Incomplete bool `json:"incomplete_files_ext"`
	}
	if json.Unmarshal(raw, &prefs) != nil {
		return ErrResponse
	}
	if !prefs.Incomplete {
		if _, err = c.request(ctx, http.MethodPost, "app/setPreferences", url.Values{"json": {`{"incomplete_files_ext":true}`}}); err != nil {
			return err
		}
	}
	automatic := mode == "auto" || mode != "manual" && prefs.Auto
	if form.Get("savepath") == "" && (automatic || mode == "manual") {
		form.Set("autoTMM", strconv.FormatBool(automatic))
	}
	if !automatic || form.Get("category") == "" {
		return nil
	}
	for _, dir := range dirs {
		if dir.Label != form.Get("category") || dir.SavePath == "" {
			continue
		}
		raw, err := c.request(ctx, http.MethodGet, "torrents/categories", nil)
		if err != nil {
			return err
		}
		var categories map[string]struct{ SavePath string }
		if json.Unmarshal(raw, &categories) != nil || categories == nil {
			return ErrResponse
		}
		category, exists := categories[dir.Label]
		if exists && filepath.Clean(category.SavePath) == filepath.Clean(dir.SavePath) {
			return nil
		}
		action := "torrents/createCategory"
		if exists {
			action = "torrents/editCategory"
		}
		_, err = c.request(ctx, http.MethodPost, action, url.Values{"category": {dir.Label}, "savePath": {dir.SavePath}})
		return err
	}
	return nil
}

// The Web API's plain "Ok." also occurs on duplicate submissions. The marker
// and the full advertised hashes tie the reply to this specific operation.
func (c *Client) AddClassic(ctx context.Context, magnet string, torrent []byte, o AddOptions, mode string, dirs []ClassicDirectory, receipt string, identity torrentmeta.TransferMetadata) (string, error) {
	form, err := o.values()
	if err != nil {
		return "", err
	}
	p, err := c.TransferCapabilities(ctx)
	if err != nil {
		return "", ErrConfiguration
	}
	id, err := p.WireID(identity)
	if err != nil {
		return "", ErrConfiguration
	}
	if _, err = c.transferInfo(ctx, id); !errors.Is(err, torrenttransfer.ErrTaskAbsent) {
		return "", ErrConfiguration
	}
	if err = c.classicManagement(ctx, form, mode, dirs); err != nil {
		return "", ErrConfiguration
	}
	form.Set("contentLayout", "Original")
	form.Del("paused")
	if p.Major == 5 {
		form.Set("stopped", strconv.FormatBool(o.Paused))
	} else {
		form.Set("paused", strconv.FormatBool(o.Paused))
	}
	marker := "NT" + receipt
	tags := form.Get("tags")
	if tags != "" {
		tags += ","
	}
	form.Set("tags", tags+marker)
	if torrent != nil {
		err = c.addTorrentForm(ctx, torrent, form, id)
	} else {
		form.Set("urls", magnet)
		var raw []byte
		raw, err = c.request(ctx, http.MethodPost, "torrents/add", form)
		if err == nil {
			err = validateAddReply(raw, id)
		}
	}
	if err != nil {
		return "", err
	}
	wait, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		found, err := c.classicTask(wait, id, marker, identity, p)
		if err != nil {
			return "", ErrResponse
		}
		if found {
			return id, nil
		}
		select {
		case <-wait.Done():
			return "", ErrResponse
		case <-tick.C:
		}
	}
}

func (c *Client) classicTask(ctx context.Context, id, marker string, identity torrentmeta.TransferMetadata, p TransferProtocol) (bool, error) {
	row, err := c.transferInfo(ctx, id)
	if errors.Is(err, torrenttransfer.ErrTaskAbsent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Tags == nil || !slices.ContainsFunc(strings.Split(*row.Tags, ","), func(s string) bool { return strings.TrimSpace(s) == marker }) {
		return false, nil
	}
	if identity.V2Hash != "" && p.V2 {
		raw, err := c.request(ctx, http.MethodGet, "torrents/properties?"+url.Values{"hash": {id}}.Encode(), nil)
		if err != nil {
			return false, err
		}
		var props struct {
			V1 string `json:"infohash_v1"`
			V2 string `json:"infohash_v2"`
		}
		if json.Unmarshal(raw, &props) != nil || !strings.EqualFold(props.V2, identity.V2Hash) || identity.V1Hash != "" && !strings.EqualFold(props.V1, identity.V1Hash) {
			return false, ErrResponse
		}
	}
	return true, nil
}

func (c *Client) VerifyClassic(ctx context.Context, receipt string, identity torrentmeta.TransferMetadata) (string, error) {
	p, err := c.TransferCapabilities(ctx)
	if err != nil {
		return "", err
	}
	id, err := p.WireID(identity)
	if err != nil {
		return "", err
	}
	found, err := c.classicTask(ctx, id, "NT"+receipt, identity, p)
	if err != nil || !found {
		return "", ErrResponse
	}
	return id, nil
}

func (c *Client) RemoveClassicMarker(ctx context.Context, id, receipt string) error {
	if !ValidHash(id) || len(receipt) != 32 {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	_, err := c.request(ctx, http.MethodPost, "torrents/removeTags", url.Values{"hashes": {id}, "tags": {"NT" + receipt}})
	return err
}
