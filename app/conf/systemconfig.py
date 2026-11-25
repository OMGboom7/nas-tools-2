import json

from config import run_go_helper

from app.helper import DictHelper
from app.utils.commons import singleton
from app.utils.types import SystemConfigKey


@singleton
class SystemConfig:
    # 系统设置
    systemconfig = {}

    def __init__(self):
        self.dicthelper = DictHelper()
        self.init_config()

    def init_config(self):
        """
        缓存系统设置
        """
        for item in self.dicthelper.list("SystemConfig"):
            if not item:
                continue
            if self.__is_obj(item.VALUE):
                self.systemconfig[item.KEY] = json.loads(item.VALUE)
            else:
                self.systemconfig[item.KEY] = item.VALUE

    @staticmethod
    def __is_obj(obj):
        if isinstance(obj, list) or isinstance(obj, dict):
            return True
        else:
            return str(obj).startswith("{") or str(obj).startswith("[")

    @staticmethod
    def read_plugin_config(plugin_id):
        return json.loads(run_go_helper("plugin-config", "read", plugin_id))

    @staticmethod
    def save_plugin_config(plugin_id, value, revision=None):
        data = {"value": value}
        if revision is not None:
            data["revision"] = revision
        run_go_helper("plugin-config", "save", plugin_id,
                      payload=json.dumps(data, allow_nan=False).encode())

    def set(self, key: [SystemConfigKey, str], value):
        """
        设置系统设置
        """
        if isinstance(key, SystemConfigKey):
            key = key.value
        if key.startswith("plugin.") and isinstance(value, dict):
            self.save_plugin_config(key[7:], value)
            self.systemconfig[key] = value
            return
        # 更新内存
        self.systemconfig[key] = value
        # 写入数据库
        if self.__is_obj(value):
            if value is not None:
                value = json.dumps(value)
            else:
                value = ''
        self.dicthelper.set("SystemConfig", key, value)

    def get(self, key: [SystemConfigKey, str] = None):
        """
        获取系统设置
        """
        if not key:
            self.init_config()
            return {**self.systemconfig, SystemConfigKey.UserInstalledPlugins.value:
                    self.get(SystemConfigKey.UserInstalledPlugins)}
        if isinstance(key, SystemConfigKey):
            key = key.value
        if key == SystemConfigKey.UserInstalledPlugins.value:
            return json.loads(run_go_helper("plugin-installation", "list"))
        if key.startswith("plugin."):
            return self.read_plugin_config(key[7:])["value"]
        return self.systemconfig.get(key)
