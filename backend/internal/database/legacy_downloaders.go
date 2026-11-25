package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

// Receipts cover the interval between committing user.db and replacing the
// YAML. A renamed, edited or deleted downloader must not be recreated on retry.
func importLegacyDownloaders(tx *sql.Tx, clients []config.LegacyDownloader) error {
	if len(clients) == 0 {
		return nil
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS GO_LEGACY_DOWNLOADER_IMPORT(TYPE TEXT PRIMARY KEY, SOURCE TEXT NOT NULL)`); err != nil {
		return err
	}
	for _, client := range clients {
		raw, err := json.Marshal(client)
		if err != nil {
			return fmt.Errorf("invalid legacy %s settings", client.Type)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(raw))
		var previous string
		err = tx.QueryRow("SELECT SOURCE FROM GO_LEGACY_DOWNLOADER_IMPORT WHERE TYPE=?", client.Type).Scan(&previous)
		if err == nil {
			if previous != digest {
				return fmt.Errorf("legacy %s settings changed after database import; retained YAML for reconciliation", client.Type)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, enabled, err := matchLegacyDownloader(tx, client)
		if err != nil {
			return err
		}
		if id == 0 {
			name := "Qbittorrent"
			if client.Type == "transmission" {
				name = "Transmission"
			}
			conn, err := json.Marshal(client.Connection)
			if err != nil {
				return fmt.Errorf("invalid legacy %s connection", client.Type)
			}
			dirs, err := json.Marshal(client.Directories)
			if err != nil {
				return fmt.Errorf("invalid legacy %s directories", client.Type)
			}
			result, err := tx.Exec(`INSERT INTO DOWNLOADER(NAME,TYPE,ENABLED,TRANSFER,ONLY_NASTOOL,MATCH_PATH,RMT_MODE,CONFIG,DOWNLOAD_DIR) VALUES(?,?,?,?,?,0,?,?,?)`, name, client.Type, client.Enabled, client.Monitor, client.OnlyNastool, client.Mode, string(conn), string(dirs))
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return fmt.Errorf("could not insert legacy %s", client.Type)
			}
			id, err = result.LastInsertId()
			if err != nil {
				return err
			}
			enabled = client.Enabled
		}
		if client.Enabled && enabled {
			if err := saveLegacyDefault(tx, id); err != nil {
				return err
			}
		}
		result, err := tx.Exec("INSERT INTO GO_LEGACY_DOWNLOADER_IMPORT(TYPE,SOURCE) VALUES(?,?)", client.Type, digest)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.New("could not record legacy downloader import")
		}
	}
	return nil
}

// With just pt_client left, only an unambiguous enabled database row can
// establish the old selection. Keep unmatched YAML for later reconciliation.
func importLegacySelection(tx *sql.Tx, kind string) (bool, error) {
	if kind == "" {
		return false, nil
	}
	var count, enabled int
	var id int64
	if err := tx.QueryRow("SELECT COUNT(*),COALESCE(MIN(ID),0),COALESCE(MIN(ENABLED),0) FROM DOWNLOADER WHERE TYPE=?", kind).Scan(&count, &id, &enabled); err != nil {
		return false, err
	}
	if count != 1 || enabled == 0 {
		return false, nil
	}
	if err := saveLegacyDefault(tx, id); err != nil {
		return false, err
	}
	return true, nil
}

func saveLegacyDefault(tx *sql.Tx, id int64) error {
	// A current selection, including an explicitly cleared one, wins.
	if _, err := tx.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE)
				SELECT 'SystemConfig','DefaultDownloader',?,''
				WHERE NOT EXISTS (SELECT 1 FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY='DefaultDownloader')`, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	var stored bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY='DefaultDownloader')").Scan(&stored); err != nil || !stored {
		return errors.New("could not record default downloader")
	}
	return nil
}

// Compare the endpoint and account, allowing the user to have changed the
// display name, password or directory mappings in the existing database row.
func matchLegacyDownloader(tx *sql.Tx, client config.LegacyDownloader) (int64, bool, error) {
	target := downloaderEndpoint(client.Connection)
	if target == "" {
		return 0, false, nil
	}
	rows, err := tx.Query("SELECT ID,COALESCE(ENABLED,0),COALESCE(CONFIG,'') FROM DOWNLOADER WHERE TYPE=?", client.Type)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var matched int64
	var enabled bool
	for rows.Next() {
		var id int64
		var flag int
		var raw string
		if err := rows.Scan(&id, &flag, &raw); err != nil {
			return 0, false, err
		}
		var current map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid([]byte(raw)) || decoder.Decode(&current) != nil || downloaderEndpoint(current) != target || downloaderScalar(current["username"]) != downloaderScalar(client.Connection["username"]) {
			continue
		}
		if matched != 0 {
			return 0, false, fmt.Errorf("multiple %s records match the legacy endpoint and account; YAML retained", client.Type)
		}
		matched, enabled = id, flag != 0
	}
	return matched, enabled, rows.Err()
}

func downloaderScalar(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func downloaderEndpoint(fields map[string]any) string {
	host := strings.TrimSpace(downloaderScalar(fields["host"]))
	if host == "" {
		return ""
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	port := strings.TrimSpace(downloaderScalar(fields["port"]))
	if port == "" {
		port = u.Port()
	}
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port) + strings.TrimRight(u.EscapedPath(), "/")
}
