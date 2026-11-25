import hashlib
import json
import subprocess
from threading import Event, Lock, Thread

import log
from app.message.client._base import _IMessageClient
from app.utils import ExceptionUtils
from config import go_helper_path, run_go_helper


def dispatch_telegram(result):
    # The application command handlers are loaded after messaging is initialized.
    from app.message import Message
    from app.utils.types import SearchType
    from web.action import WebAction

    if result["action"] not in ("deny", "dispatch"):
        return "Ok"
    channel = Message().get_interactive_client(SearchType.TG)
    if not channel or channel["id"] != result["channel"]:
        return "Ok"
    token = channel["config"].get("token") or ""
    if hashlib.sha256(token.encode()).hexdigest() != result["token_digest"]:
        return "Ok"
    if result["action"] == "deny":
        channel["client"].send_msg(title=result["text"], user_id=result["user_id"])
        return result["text"]
    if result["action"] == "dispatch":
        WebAction().handle_message_job(msg=result["text"], in_from=SearchType.TG,
                                       user_id=result["user_id"], user_name=result.get("name"))
    return "Ok"


class Telegram(_IMessageClient):
    schema = "telegram"

    def __init__(self, config):
        self._telegram_token = config.get("token")
        self._telegram_chat_id = config.get("chat_id")
        self._telegram_thread_id = config.get("thread_id")

    @classmethod
    def match(cls, ctype):
        return True if ctype == cls.schema else False

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


class TelegramReceiver:
    def __init__(self):
        self._state = Lock()
        self._stopped = None
        self._worker = None

    def start(self):
        with self._state:
            if self._stopped is not None:
                return
            self._stopped = Event()
            Thread(target=self._receive, args=(self._stopped,), daemon=True, name="telegram-receiver").start()

    def _receive(self, stopped):
        while not stopped.is_set():
            try:
                with self._state:
                    if stopped.is_set():
                        return
                    worker = subprocess.Popen([go_helper_path(), "telegram-receiver"], stdin=subprocess.PIPE,
                                              stdout=subprocess.PIPE, text=True, encoding="utf-8")
                    self._worker = worker
                try:
                    for line in worker.stdout:
                        if stopped.is_set():
                            break
                        try:
                            dispatch_telegram(json.loads(line))
                        except Exception:
                            log.error("【Telegram】交互命令的执行结果未确认")
                finally:
                    with self._state:
                        if self._worker is worker:
                            self._worker = None
                        worker.stdin.close()
                    self._wait(worker)
                    worker.stdout.close()
            except (OSError, ValueError, subprocess.SubprocessError):
                log.error("【Telegram】接收程序暂不可用")
            stopped.wait(5)

    @staticmethod
    def _wait(worker):
        try:
            worker.wait(timeout=8)
        except subprocess.TimeoutExpired:
            worker.kill()
            worker.wait()

    def stop(self):
        with self._state:
            if self._stopped is None:
                return
            self._stopped.set()
            self._stopped = None
            worker = self._worker
            self._worker = None
            if worker:
                worker.stdin.close()
        if worker:
            self._wait(worker)
