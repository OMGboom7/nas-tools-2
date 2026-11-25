import json

from app.sites.siteuserinfo._base import _ISiteUserInfo, SITE_BASE_ORDER
from app.utils import MteamUtils
from app.utils.types import SiteSchema


class MteamSiteUserInfo(_ISiteUserInfo):

    def _parse_message_unread_links(self, html_text, msg_links):
        pass

    def _parse_site_page(self, html_text):
        pass

    def _parse_user_base_info(self, html_text):
        pass

    def _parse_user_traffic_info(self, html_text):
        pass

    def _parse_user_torrent_seeding_info(self, html_text, multi_page=False):
        pass

    def _parse_user_detail_info(self, html_text):
        pass

    def _parse_message_content(self, html_text):
        pass

    api_key = None
    schema = SiteSchema.Mteam
    order = SITE_BASE_ORDER

    @classmethod
    def match(cls, html_text):
        """
        默认使用NexusPhp解析
        :param html_text:
        :return:
        """
        return "M-Team" in html_text

    def _parse_logged_in(self, html_text):
        ok, message = MteamUtils.test_connection({
            "signurl": self.site_url, "api_key": self.api_key or MteamUtils.get_api_key(self.site_url),
            "ua": self._ua, "proxy": self._proxy,
        })
        return (True, "获取用户信息成功") if ok else (False, message)

    def parse(self):
        key = self.api_key if self.api_key is not None else MteamUtils.get_api_key(self.site_url)
        data = MteamUtils.statistics(self.site_url, key, self._ua, self._proxy)
        if not data:
            self.err_msg = "M-Team 账户或做种数据获取失败"
            return
        self.err_msg = None
        self.userid = data["userId"]
        self.username = data["username"]
        self.user_level = data["userLevel"]
        self.join_at = data["joinAt"]
        self.last_seen = data["lastSeen"]
        self.upload = data["upload"]
        self.download = data["download"]
        self.ratio = data["ratio"]
        self.bonus = data["bonus"]
        self.seeding = data["seeding"]
        self.seeding_size = data["seedingSize"]
        self.seeding_info = json.dumps(data["seedingInfo"])
        self.site_favicon = data.get("favicon", "").partition(",")[2]
