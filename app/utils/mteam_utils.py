import json
import subprocess
from urllib.parse import urlparse

from app.utils import RequestUtils
from config import Config, run_go_helper


class MteamUtils:

    @staticmethod
    def get_api_url(url):
        parsed_url = urlparse(url)
        index = parsed_url.hostname.index('m-team')
        last_domain = parsed_url.hostname[index:]
        return str(parsed_url.scheme) + "://" + 'api.' + last_domain

    @staticmethod
    def is_site(url):
        try:
            host = urlparse(url).hostname or ""
        except ValueError:
            return False
        return host in ("kp.m-team.info", "m-team.cc", "m-team.io") or host.endswith((".m-team.cc", ".m-team.io"))

    @staticmethod
    def _inspect(command, url, key, ua, proxy):
        proxy_url = (Config().get_proxies() or {}).get("https") if proxy else None
        if proxy and not proxy_url:
            return {}
        query = {"url": url, "key": key, "ua": ua or Config().get_ua(), "proxy": proxy_url}
        try:
            reply = run_go_helper(command, payload=json.dumps(query).encode(), timeout=20)
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
    def buildRequestUtils(cookies=None, api_key=None, headers=None, proxies=False, content_type=None, accept_type=None, session=None, referer=None, timeout=30):
        if api_key:
            # use api key
            return RequestUtils(headers=headers, api_key=api_key, timeout=timeout, referer=referer,
                                content_type=content_type, session=session, accept_type=accept_type,
                                proxies=Config().get_proxies() if proxies else None)
        return RequestUtils(headers=headers, cookies=cookies, timeout=timeout, referer=referer,
                            content_type=content_type, session=session, accept_type=accept_type,
                            proxies=Config().get_proxies() if proxies else None)

