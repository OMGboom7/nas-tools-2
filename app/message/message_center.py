import json
import subprocess

import log
from config import run_go_helper


class MessageCenter:
    def insert_system_message(self, title, content=None):
        data = {"title": title or "", "content": content or ""}
        try:
            run_go_helper("message-center", "append", payload=json.dumps(data, ensure_ascii=False).encode())
        except (OSError, ValueError, subprocess.SubprocessError):
            log.error("【Message】系统消息写入失败")

    def get_system_messages(self, num=20, lst_time=None, cursor=None):
        try:
            query = {"limit": num, "since": lst_time or "", "after": int(cursor or 0)}
            return json.loads(run_go_helper("message-center", "list", payload=json.dumps(query).encode()))
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            log.error("【Message】系统消息读取失败")
            return []
