import json
import subprocess
from urllib.parse import urlparse

from config import Config, run_go_helper


class MteamUtils:

    @staticmethod
    def is_site(url):
        try:
            host = urlparse(url).hostname or ""
        except ValueError:
            return False
        return host in ("kp.m-team.info", "m-team.cc", "m-team.io") or host.endswith((".m-team.cc", ".m-team.io"))

    @staticmethod
    def _inspect(command, url, key, ua, proxy, timeout=20):
        proxy_url = (Config().get_proxies() or {}).get("https") if proxy else None
        if proxy and not proxy_url:
            return {}
        query = {"url": url, "key": key, "ua": ua or Config().get_ua(), "proxy": proxy_url}
        try:
            reply = run_go_helper(command, payload=json.dumps(query).encode(), timeout=timeout)
            return json.loads(reply)
        except (OSError, ValueError, subprocess.SubprocessError):
            return {}

    @staticmethod
    def torrent_attributes(url, ua=None, proxy=False):
        result = MteamUtils._inspect("mteam-detail", url, MteamUtils.get_api_key(url), ua, proxy)
        if "free" in result:
            return result
        return None

    @staticmethod
    def test_connection(site_info):
        url = site_info.get("signurl") or site_info.get("rssurl")
        key = site_info.get("api_key")
        if not key:
            return False, "未配置站点API Key"
        result = MteamUtils._inspect("mteam-profile", url, key, site_info.get("ua"), site_info.get("proxy"))
        if result.get("ok"):
            return True, "连接成功"
        if "status" in result:
            return False, f"连接失败，状态码：{result['status']}"
        return False, "连接失败"

    @staticmethod
    def get_api_key(url):
        from app.sites import Sites
        sites = Sites()
        site_info = sites.get_sites(siteurl=url)
        if site_info:
            api_key = site_info.get("api_key")

            if api_key:
                return api_key

        return None

    @staticmethod
    def statistics(url, key, ua, proxy):
        # Go limits each request and the number of pages in the complete snapshot.
        return MteamUtils._inspect("mteam-statistics", url, key, ua, proxy, timeout=None)
