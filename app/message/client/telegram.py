import json
import subprocess
from threading import Event, Lock
from urllib.parse import urlencode

import requests

import log
from app.helper import ThreadHelper
from app.message.client._base import _IMessageClient
from app.utils import RequestUtils, ExceptionUtils
from config import Config, run_go_helper

lock = Lock()
WEBHOOK_STATUS = False


class Telegram(_IMessageClient):
    schema = "telegram"

    _telegram_token = None
    _telegram_chat_id = None
    _telegram_thread_id = None
    _webhook = None
    _webhook_url = None
    _telegram_user_ids = []
    _telegram_admin_ids = []
    _domain = None
    _message_proxy_event = None
    _client_config = {}
    _interactive = False
    _enabled = True
    _api_key = None
    _telegram_domain = None

    def __init__(self, config):
        self._client_config = config
        self._interactive = config.get("interactive")
        self._domain = Config().get_domain()
        self._api_key = Config().get_config("security").get("api_key")
        self._telegram_domain = Config().get_telegram_domain()
        self.init_config()

    def init_config(self):
        if self._client_config:
            self._telegram_token = self._client_config.get('token')
            self._telegram_chat_id = self._client_config.get('chat_id')
            self._telegram_thread_id = self._client_config.get('thread_id')
            self._webhook = self._client_config.get('webhook')
            telegram_admin_ids = self._client_config.get('admin_ids')
            if telegram_admin_ids:
                self._telegram_admin_ids = telegram_admin_ids.split(",")
            self._telegram_user_ids = self._telegram_admin_ids
            telegram_user_ids = self._client_config.get('user_ids')
            if telegram_user_ids:
                self._telegram_user_ids.extend(telegram_user_ids.split(","))
            if self._telegram_token and self._telegram_chat_id:
                if self._webhook:
                    if self._domain:
                        self._webhook_url = "%s/telegram?apikey=%s" % (
                            self._domain, self._api_key)
                        self.__set_bot_webhook()
                    if self._message_proxy_event:
                        self._message_proxy_event.set()
                        self._message_proxy_event = None
                elif self._interactive:
                    self.__del_bot_webhook()
                    if not self._message_proxy_event:
                        event = Event()
                        self._message_proxy_event = event
                        ThreadHelper().start_thread(
                            self.__start_telegram_message_proxy, [event])

    @classmethod
    def match(cls, ctype):
        return True if ctype == cls.schema else False

    def get_admin(self):
        """
        获取允许使用远程命令的user_id列表
        """
        return self._telegram_admin_ids

    def get_users(self):
        """
        获取允许使用telegram机器人的user_id列表
        """
        return self._telegram_user_ids

    def _send(self, kind, user_id, thread_id, **fields):
        fields.update(token=self._telegram_token, chat_id=str(user_id or self._telegram_chat_id),
                      thread_id=str(thread_id or self._telegram_thread_id or ""))
        try:
            payload = json.dumps(fields, ensure_ascii=False).encode()
            run_go_helper("send-telegram", kind, payload=payload, timeout=70)
            return True, ""
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            return False, "Telegram 消息发送失败或未收到确认"

    def send_msg(self, title, text="", image="", url="", user_id="", thread_id=""):
        if not title and not text:
            return False, "标题和内容不能同时为空"
        if not self._telegram_token or not self._telegram_chat_id:
            return False, "参数未配置"
        return self._send("single", user_id, thread_id, title=title, text=text, image=image, url=url)

    def send_list_msg(self, medias: list, user_id="", title="", thread_id="", **kwargs):
        if not self._telegram_token or not self._telegram_chat_id:
            return False, "参数未配置"
        if not title or not isinstance(medias, list):
            return False, "数据错误"
        items, image = [], ""
        try:
            for media in medias:
                if not image:
                    image = media.get_message_image()
                items.append({"title": media.get_title_string(), "url": media.get_detail_url(),
                              "type": media.get_type_string(), "vote": media.get_vote_string()})
        except Exception as exc:
            ExceptionUtils.exception_traceback(exc)
            return False, "Telegram 媒体列表读取失败"
        return self._send("list", user_id, thread_id, title=title, image=image, medias=items)

    def __set_bot_webhook(self):
        """
        设置Telegram Webhook
        """
        if not self._webhook_url:
            return

        try:
            lock.acquire()
            global WEBHOOK_STATUS
            if not WEBHOOK_STATUS:
                WEBHOOK_STATUS = True
            else:
                return
        finally:
            lock.release()

        status = self.__get_bot_webhook()
        if status and status != 1:
            if status == 2:
                self.__del_bot_webhook()
            values = {"url": self._webhook_url, "allowed_updates": ["message"]}
            sc_url = "%s/bot%s/setWebhook?" % (
                self._telegram_domain, self._telegram_token)
            res = RequestUtils(proxies=Config().get_proxies()).get_res(
                sc_url + urlencode(values))
            if res is not None:
                json = res.json()
                if json.get("ok"):
                    log.info("【Telegram】Webhook 设置成功，地址为：%s" %
                             self._webhook_url)
                else:
                    log.error("【Telegram】Webhook 设置失败：%s" %
                              json.get("description"))
            else:
                log.error("【Telegram】Webhook 设置失败：网络连接故障！")

    def __get_bot_webhook(self):
        """
        获取Telegram已设置的Webhook
        :return: 状态：1-存在且相等，2-存在不相等，3-不存在，0-网络出错
        """
        sc_url = "%s/bot%s/getWebhookInfo" % (
            self._telegram_domain, self._telegram_token)
        res = RequestUtils(proxies=Config().get_proxies()).get_res(sc_url)
        if res is not None and res.json():
            if res.json().get("ok"):
                result = res.json().get("result") or {}
                webhook_url = result.get("url") or ""
                if webhook_url:
                    log.info("【Telegram】Webhook 地址为：%s" % webhook_url)
                pending_update_count = result.get("pending_update_count")
                last_error_message = result.get("last_error_message")
                if pending_update_count and last_error_message:
                    log.warn("【Telegram】Webhook 有 %s 条消息挂起，最后一次失败原因为：%s" % (
                        pending_update_count, last_error_message))
                if webhook_url == self._webhook_url:
                    return 1
                else:
                    return 2
            else:
                return 3
        else:
            return 0

    def __del_bot_webhook(self):
        """
        删除Telegram Webhook
        :return: 是否成功
        """
        sc_url = "%s/bot%s/deleteWebhook" % (
            self._telegram_domain, self._telegram_token)
        res = RequestUtils(proxies=Config().get_proxies()).get_res(sc_url)
        if res and res.json() and res.json().get("ok"):
            return True
        else:
            return False

    def __start_telegram_message_proxy(self, event: Event):
        log.info("Telegram消息接收服务启动")

        long_poll_timeout = 5

        def consume_messages(_config, _offset, _sc_url, _ds_url):
            try:
                values = {"timeout": long_poll_timeout, "offset": _offset}
                res = RequestUtils(proxies=_config.get_proxies()).get_res(
                    _sc_url + urlencode(values))
                # log.debug("【Telegram, request: " + (_sc_url + urlencode(values)))
                if res:
                    # log.debug("【Telegram, res: " + (str(res.content)))
                    if res.json():
                        for msg in res.json().get("result", []):
                            # 无论本地是否成功，先更新offset，即消息最多成功消费一次
                            _offset = msg["update_id"] + 1
                            log.debug("【Telegram】接收到消息: %s" % msg)
                            local_res = requests.post(
                                _ds_url, json=msg, timeout=10)
                            log.debug("【Telegram】message: %s processed, response is: %s" % (
                                msg, local_res.text))
            except Exception as e:
                ExceptionUtils.exception_traceback(e)
                log.error("【Telegram】消息接收出现错误: %s" % e)
            return _offset

        offset = 0
        while True:
            _config = Config()
            web_port = _config.get_config("app").get("web_port")
            sc_url = "%s/bot%s/getUpdates?" % (
                self._telegram_domain, self._telegram_token)
            ds_url = "http://127.0.0.1:%s/telegram?apikey=%s" % (
                web_port, self._api_key)
            if not self._enabled:
                log.info("Telegram消息接收服务已停止")
                break

            i = 0
            while i < 20 and not event.is_set():
                offset = consume_messages(_config, offset, sc_url, ds_url)
                i = i + 1

    def stop_service(self):
        """
        停止服务
        """
        self._enabled = False
