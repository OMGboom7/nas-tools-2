import json

from app.utils.types import MediaType
from config import run_go_helper


def _request(operation, **fields):
    payload = json.dumps(fields, ensure_ascii=False).encode("utf-8")
    return json.loads(run_go_helper("recognition-cache", operation, payload=payload, timeout=120))


class MetaHelper:
    """Compatibility interface for callers that still use Python media objects."""

    def clear_meta_data(self):
        _request("clear")

    def get_meta_data_by_key(self, key):
        entry = _request("get", key=key) if key else None
        if not entry:
            return {}
        if entry.get("type") in MediaType._value2member_map_:
            entry["type"] = MediaType(entry["type"])
        return entry

    def dump_meta_data(self, search, page, num):
        result = _request("list", search=search or "", page=page, size=num)
        items = []
        for row in result["items"]:
            key, entry = row["key"], row["entry"]
            info = {field: entry.get(field) for field in ("id", "title", "year", "poster_path", "backdrop_path")}
            info["media_type"] = entry.get("type")
            display = key.replace("[电影]", "").replace("[电视剧]", "").replace("[未知]", "").replace("-None", "")
            items.append((key, info, display))
        return result["total"], items

    def delete_meta_data(self, key):
        return _request("delete", key=key) if key else None

    def delete_meta_data_by_tmdbid(self, tmdbid):
        if tmdbid is not None:
            _request("delete-id", id=str(tmdbid))

    def delete_unknown_meta(self):
        _request("clear-unknown")

    def modify_meta_data(self, key, title):
        return _request("rename", key=key, title=title) if key else None

    def update_meta_data(self, meta_data):
        if meta_data:
            entries = {key: dict(entry) for key, entry in meta_data.items()}
            for entry in entries.values():
                if isinstance(entry.get("type"), MediaType):
                    entry["type"] = entry["type"].value
            _request("put", entries=entries)

    def get_cache_title(self, key):
        entry = self.get_meta_data_by_key(key)
        return entry.get("title") if entry.get("id") else None

    def set_cache_title(self, key, cn_title):
        self.modify_meta_data(key, cn_title)
