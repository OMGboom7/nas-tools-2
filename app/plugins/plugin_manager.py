import hashlib
import json
import os.path
import traceback
from threading import Thread

import log
from app.conf import SystemConfig
from app.helper import SubmoduleHelper
from app.plugins.event_manager import EventManager
from app.utils import SystemUtils, PathUtils, ImageUtils
from app.utils.commons import singleton
from app.utils.types import SystemConfigKey
from config import Config, run_go_helper


@singleton
class PluginManager:
    """
    插件管理器
    """
    systemconfig = None
    eventmanager = None

    # 用户插件目录
    user_plugin_path = None
    # 内部插件目录
    system_plugin_path = None

    # 插件列表
    _plugins = {}
    # 运行态插件列表
    _running_plugins = {}
    # 配置Key
    _config_key = "plugin.%s"
    # 事件处理线程
    _thread = None
    # 开关
    _active = False

    def __init__(self):
        # config/plugins 是插件py文件目录，config/plugins/xxx是插件数据目录
        self.user_plugin_path = Config().get_user_plugin_path()
        if not os.path.exists(self.user_plugin_path):
            os.makedirs(self.user_plugin_path, exist_ok=True)
        self.system_plugin_path = os.path.join(os.path.dirname(os.path.realpath(__file__)), "modules")
        self._external_modules = {}
        self._external_digests = {}
        if os.path.exists(self.user_plugin_path):
            for plugin_file in PathUtils.get_dir_level1_files(self.user_plugin_path, [".py"]):
                status, _ = SystemUtils.copy(plugin_file, self.system_plugin_path)
                if status == 0:
                    name = os.path.basename(plugin_file)
                    self._external_modules["app.plugins.modules." + os.path.splitext(name)[0]] = name
                    try:
                        with open(os.path.join(self.system_plugin_path, name), "rb") as module:
                            digest = hashlib.sha256()
                            for chunk in iter(lambda: module.read(65536), b""):
                                digest.update(chunk)
                        self._external_digests[name] = digest.hexdigest()
                    except OSError:
                        pass
        self.init_config()

    def init_config(self):
        self.systemconfig = SystemConfig()
        self.eventmanager = EventManager()
        # 停止已有插件
        self.stop_service()
        # 启动插件
        self.start_service()

    def __run(self):
        """
        事件处理线程
        """
        while self._active:
            event, handlers = self.eventmanager.get_event()
            if event:
                log.info(f"处理事件：{event.event_type} - {handlers}")
                for handler in handlers:
                    try:
                        names = handler.__qualname__.split(".")
                        self.run_plugin(names[0], names[1], event)
                    except Exception as e:
                        log.error(f"事件处理出错：{str(e)} - {traceback.format_exc()}")

    def start_service(self):
        """
        启动
        """
        # 加载插件
        self.__load_plugins()
        # 将事件管理器设为启动
        self._active = True
        self._thread = Thread(target=self.__run)
        # 启动事件处理线程
        self._thread.start()

    def stop_service(self):
        """
        停止
        """
        # 将事件管理器设为停止
        self._active = False
        # 等待事件处理线程退出
        if self._thread:
            self._thread.join()
        # 停止所有插件
        self.__stop_plugins()

    def __load_plugins(self):
        """
        加载所有插件
        """
        # 扫描插件目录
        plugins = SubmoduleHelper.import_submodules(
            "app.plugins.modules",
            filter_func=lambda _, obj: hasattr(obj, 'module_name')
        )
        # 排序
        plugins.sort(key=lambda x: x.module_order if hasattr(x, "module_order") else 0)
        # 用户已安装插件列表
        user_plugins = self.systemconfig.get(SystemConfigKey.UserInstalledPlugins) or []
        self._running_plugins = {}
        self._plugins = {}
        for plugin in plugins:
            module_id = plugin.__name__
            if module_id == "DiskSpaceSaver" and os.environ.get("NASTOOL_DISK_SPACE_OWNER") == "go":
                continue
            if module_id == "MteamRssGen" and os.environ.get("NASTOOL_MTEAM_RSS_OWNER") == "go":
                continue
            self._plugins[module_id] = plugin
            # 未安装的跳过加载
            if module_id not in user_plugins:
                continue
            # 生成实例
            self._running_plugins[module_id] = plugin()
            # 初始化配置
            self.reload_plugin(module_id)
            log.info(f"加载插件：{plugin}")

        # Keep the defining module of each discovered class for a later Go-only upgrade.
        sources = {}
        catalog = {}
        for pid, plugin in self._plugins.items():
            name = self._external_modules.get(plugin.__module__)
            if name:
                sources.setdefault(name, []).append(pid)
                digest = self._external_digests.get(name)
                if digest:
                    catalog[pid] = {**self._market_declaration(pid, plugin), "source": name, "digest": digest}
        if catalog != (self.systemconfig.get("GoExternalPluginCatalog") or {}):
            self.systemconfig.set("GoExternalPluginCatalog", catalog)
        for ids in sources.values():
            ids.sort()
        if sources != (self.systemconfig.get("GoExternalPluginSources") or {}):
            self.systemconfig.set("GoExternalPluginSources", sources)

    def run_plugin(self, pid, method, *args, **kwargs):
        """
        运行插件
        """
        if not self._running_plugins.get(pid):
            return None
        if not hasattr(self._running_plugins[pid], method):
            return
        try:
            return getattr(self._running_plugins[pid], method)(*args, **kwargs)
        except Exception as err:
            print(str(err), traceback.format_exc())

    def reload_plugin(self, pid, require_running=False):
        """
        生效插件配置
        """
        plugin = self._running_plugins.get(pid)
        if plugin is None:
            return not require_running
        if not hasattr(plugin, "init_config"):
            return True
        try:
            plugin.init_config(self.get_plugin_config(pid))
            log.debug(f"生效插件配置：{pid}")
            return True
        except Exception:
            log.error(f"插件配置未能生效：{pid}")
            return False

    def __stop_plugins(self):
        """
        停止所有插件
        """
        for plugin in self._running_plugins.values():
            if hasattr(plugin, "stop_service"):
                plugin.stop_service()

    def get_plugin_config(self, pid):
        """
        获取插件配置
        """
        if not self._plugins.get(pid):
            return {}
        return self.systemconfig.get(self._config_key % pid) or {}

    def get_plugin_page(self, pid):
        """
        获取插件额外页面数据
        :return: 标题，页面内容，确定按钮响应函数
        """
        if not self._running_plugins.get(pid):
            return None, None, None
        if not hasattr(self._running_plugins[pid], "get_page"):
            return None, None, None
        return self._running_plugins[pid].get_page()

    def get_plugin_script(self, pid):
        """
        获取插件额外脚本
        """
        if not self._running_plugins.get(pid):
            return None
        if not hasattr(self._running_plugins[pid], "get_script"):
            return None
        return self._running_plugins[pid].get_script()

    def get_plugin_state(self, pid):
        """
        获取插件状态
        """
        if not self._running_plugins.get(pid):
            return None
        if not hasattr(self._running_plugins[pid], "get_state"):
            return None
        return self._running_plugins[pid].get_state()

    def save_plugin_config(self, pid, conf, auth_level=None, revision=None):
        """
        保存插件配置
        """
        plugin = self._plugins.get(pid)
        if plugin is None or (auth_level is not None and getattr(plugin, "auth_level", 0) > auth_level):
            return False
        self.systemconfig.save_plugin_config(pid, conf, revision)
        return True

    @staticmethod
    def __get_plugin_color(plugin):
        """
        获取插件的主题色
        """
        if hasattr(plugin, "module_color") and plugin.module_color:
            return plugin.module_color
        if hasattr(plugin, "module_icon"):
            icon_path = os.path.join(Config().get_root_path(),
                                     "web", "static", "img", "plugins",
                                     plugin.module_icon)
            return ImageUtils.calculate_theme_color(icon_path)
        return ""

    def get_plugins_conf(self, auth_level, selected=None):
        visible = [(pid, plugin, getattr(plugin, "auth_level", 0))
                   for pid, plugin in self._running_plugins.items()
                   if (selected is None or pid == selected) and getattr(plugin, "auth_level", 0) <= auth_level]
        external = {}
        if auth_level >= 2:
            for pid, plugin, level in visible:
                source = self._external_modules.get(plugin.__class__.__module__)
                digest = self._external_digests.get(source)
                if digest:
                    external[pid] = {"id": pid, "level": level, "digest": digest}
        revisions = {}
        if external:
            payload = json.dumps({"revisionsOnly": True, "forms": list(external.values())}).encode()
            revisions = json.loads(run_go_helper("plugin-editors", str(auth_level), payload=payload))
        forms = []
        for pid, plugin, level in visible:
            entry = {"id": pid, "level": level}
            if pid in external:
                if pid not in revisions:
                    continue
                entry.update(digest=external[pid]["digest"], revision=revisions[pid])
            attributes = {"module_name": "name", "module_desc": "desc", "module_version": "version",
                          "module_icon": "icon", "module_config_prefix": "prefix"}
            detail = {key: getattr(plugin, attr) for attr, key in attributes.items() if hasattr(plugin, attr)}
            if hasattr(plugin, "get_page"):
                detail["page"], _, _ = plugin.get_page()
            if hasattr(plugin, "get_script"):
                detail["script"] = plugin.get_script()
            detail.update(color=self.__get_plugin_color(plugin), fields=plugin.get_fields() or {},
                          state=plugin.get_state())
            entry["detail"] = detail
            forms.append(entry)
        payload = json.dumps({"selected": selected, "forms": forms}, allow_nan=False).encode()
        rows = json.loads(run_go_helper("plugin-editors", str(auth_level), payload=payload))
        return {row["id"]: row["detail"] for row in rows}

    def _market_declaration(self, pid, plugin):
        attributes = {"module_name": "name", "module_desc": "desc", "module_version": "version",
                      "module_icon": "icon", "module_author": "author", "author_url": "author_url"}
        fields = {key: getattr(plugin, attr) for attr, key in attributes.items() if hasattr(plugin, attr)}
        fields["id"] = pid
        try:
            fields["color"] = self.__get_plugin_color(plugin) if fields.get("icon") or getattr(plugin, "module_color", "") else ""
        except (OSError, ValueError):
            fields["color"] = ""
        return {"level": getattr(plugin, "auth_level", 0), "fields": fields}

    def get_plugin_apps(self, auth_level, with_runtime=False):
        declarations = [self._market_declaration(pid, plugin) for pid, plugin in self._plugins.items()]
        if with_runtime:
            for declaration in declarations:
                pid = declaration["fields"]["id"]
                plugin = self._running_plugins.get(pid)
                if plugin is None or declaration["level"] > auth_level:
                    continue
                fields = plugin.get_fields()
                page = plugin.get_page()[0] if hasattr(plugin, "get_page") else None
                state = plugin.get_state()
                running = state.lower() in ("true", "1") if isinstance(state, str) else bool(state)
                if isinstance(fields, str):
                    try:
                        fields = json.loads(fields)
                    except ValueError:
                        fields = None
                declaration["runtime"] = {"running": running, "configurable": isinstance(fields, (list, dict)) and bool(fields),
                                          "hasPage": bool(page and str(page).strip())}
        rows = run_go_helper("plugin-market", str(auth_level), payload=json.dumps(declarations).encode())
        return {row["id"]: row for row in json.loads(rows)}

    def get_plugin_commands(self):
        """
        获取插件命令
        [{
            "cmd": "/xx",
            "event": EventType.xx,
            "desc": "xxxx",
            "data": {}
        }]
        """
        ret_commands = []
        for _, plugin in self._running_plugins.items():
            if hasattr(plugin, "get_command"):
                ret_commands.append(plugin.get_command())
        return ret_commands

    def run_plugin_method(self, pid, method, *args, **kwargs):
        """
        运行插件方法
        """
        if not self._running_plugins.get(pid):
            return None
        if not hasattr(self._running_plugins[pid], method):
            return None
        return getattr(self._running_plugins[pid], method)(*args, **kwargs)
