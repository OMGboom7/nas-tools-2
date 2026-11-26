import json
import subprocess

from app.utils import StringUtils
from config import run_go_helper


def _history(operation, **fields):
    result = run_go_helper("rss-history", operation, payload=json.dumps(fields).encode(), timeout=30)
    return json.loads(result)


class RssHelper:

    @staticmethod
    def parse_rssxml(url, proxy=False, exclude_recorded=False):
        """Fetch a site feed. An expired feed is reported separately from an empty feed."""
        if not url:
            return []
        try:
            request = {"url": url, "proxy": bool(proxy), "excludeRecorded": exclude_recorded}
            reply = run_go_helper("site-feed", payload=json.dumps(request).encode(), timeout=30)
            articles = json.loads(reply)
        except (OSError, ValueError, subprocess.SubprocessError):
            return []
        for article in articles or []:
            if article["pubdate"]:
                article["pubdate"] = StringUtils.get_time_stamp(article["pubdate"])
        return articles

    @staticmethod
    def insert_rss_torrents(media_info):
        _history("insert-marker", marker={"name": media_info.org_string, "enclosure": media_info.enclosure,
                                         "type": media_info.type.value, "title": media_info.title,
                                         "year": str(media_info.year) if media_info.year is not None else None,
                                         "season": media_info.get_season_string(),
                                         "episode": media_info.get_episode_string()})

    @staticmethod
    def is_rssd_by_enclosure(enclosure):
        return not enclosure or _history("has-marker", marker={"enclosure": enclosure})

    @staticmethod
    def is_rssd_by_simple(torrent_name, enclosure):
        return (not torrent_name and not enclosure) or _history("has-marker", marker={"name": torrent_name,
                                                                                     "enclosure": enclosure})

    @staticmethod
    def simple_insert_rss_torrents(title, enclosure):
        _history("insert-marker", marker={"name": title, "enclosure": enclosure})

    @staticmethod
    def simple_delete_rss_torrents(title, enclosure=None):
        _history("remove-marker", marker={"name": title, "enclosure": enclosure})

    @staticmethod
    def truncate_rss_history():
        _history("clear-markers")

    @staticmethod
    def clear_subscription_cache():
        _history("clear-cache")
