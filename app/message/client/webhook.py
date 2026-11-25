import json
import subprocess

from app.message.client._base import _IMessageClient
from config import run_go_helper


class Webhook(_IMessageClient):
    schema = "webhook"

    def __init__(self, config):
        self._client_config = config or {}

    @classmethod
    def match(cls, ctype):
        return ctype == cls.schema

    def _send(self, kind, variables):
        try:
            payload = json.dumps({"config": self._client_config, "variables": variables}, ensure_ascii=False)
            run_go_helper("send-webhook", kind, payload=payload.encode(), timeout=25)
            return True, ""
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            return False, "Webhook 消息渲染或投递失败"

    def send_msg(self, title, text="", image="", url="", user_id=""):
        return self._send("single", {"title": title, "text": text, "image": image, "url": url, "user_id": user_id})

    def send_list_msg(self, medias: list, user_id="", title="", **kwargs):
        if not title or not medias or not isinstance(medias, list):
            return False, "标题或媒体列表为空"
        try:
            items = [{"title": media.get_title_string(), "url": media.get_detail_url(),
                      "type": media.get_type_string(), "vote": media.get_vote_string()} for media in medias]
        except Exception:
            return False, "Webhook 媒体列表读取失败"
        return self._send("list", {"title": title, "user_id": user_id, "medias": items})
