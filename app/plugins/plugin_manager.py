import hashlib
import json
import os.path
import traceback
from threading import RLock, Thread

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
        self._locks = {}
        self._retiring = {}
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

    @staticmethod
    def _go_indexer(pid):
        return pid in ("Jackett", "Prowlarr") and os.environ.get("NASTOOL_INDEXER_PLUGINS_OWNER") == "go"

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
            self._locks.setdefault(module_id, RLock())

        for pid in self._plugins:
            if pid not in user_plugins or self._go_indexer(pid):
                continue
            try:
                plan = json.loads(run_go_helper("plugin-installation", "restore", pid))
                self._apply_plan(pid, plan)
            except Exception:
                log.error(f"插件未能加载：{pid}")

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
        try:
            return self._call_instance(pid, method, *args, **kwargs)
        except Exception as err:
            print(str(err), traceback.format_exc())

    def _call_instance(self, pid, method, *args, **kwargs):
        lock = self._locks.get(pid)
        if lock is None or pid not in self._running_plugins:
            return None
        with lock:
            callback = getattr(self._running_plugins.get(pid), method, None)
            if callback is not None:
                return callback(*args, **kwargs)

    def reload_plugin(self, pid, require_running=False):
        lock = self._locks.get(pid)
        if lock is None:
            return not require_running
        with lock:
            plugin = self._running_plugins.get(pid)
            if plugin is None:
                return not require_running
            try:
                if hasattr(plugin, "init_config"):
                    plugin.init_config(self.get_plugin_config(pid))
                return True
            except Exception:
                log.error(f"插件配置未能生效：{pid}")
                return False

    def change_installation(self, pid, install, level):
        plugin = self._plugins.get(pid)
        required_level = getattr(plugin, "auth_level", 0) if plugin else 2
        if level is None or required_level > level:
            return {"code": 404, "msg": "未找到插件"}
        operation = "install" if install else "uninstall"
        if plugin is None:
            # Keep installation records for unavailable classes and Go-owned plugins.
            run_go_helper("plugin-installation", operation, pid)
        else:
            with self._locks[pid]:
                if not self._active:
                    return {"code": 1, "msg": "插件服务正在停止"}
                try:
                    self._finish_stop(pid)
                except Exception:
                    return {"code": 1, "msg": "插件未能停止，请重试"}
                state = "running" if pid in self._running_plugins else "idle"
                if self._go_indexer(pid):
                    state = "unavailable"
                plan = json.loads(run_go_helper("plugin-installation", "apply", operation, pid, state))
                try:
                    self._apply_plan(pid, plan)
                except Exception:
                    return {"code": 1, "msg": "安装状态已保存，插件未能启动或停止，请重试"}
        return {"code": 0, "msg": "插件安装成功" if install else "插件卸载成功"}

    def _apply_plan(self, pid, plan):
        if plan["action"] == "stop":
            self._retiring[pid] = self._running_plugins.pop(pid)
            self._finish_stop(pid)
        elif plan["action"] == "start":
            instance = self._plugins[pid]()
            try:
                if hasattr(instance, "init_config"):
                    instance.init_config(plan["config"] or {})
            except Exception:
                self._retiring[pid] = instance
                self._finish_stop(pid)
                raise
            self._running_plugins[pid] = instance
        if plan["receipt"]:
            run_go_helper("plugin-installation", "ack", pid, plan["receipt"])

    def _finish_stop(self, pid):
        # Retain the instance until cleanup succeeds so retries cannot start a duplicate.
        instance = self._retiring.get(pid)
        if instance is not None:
            if hasattr(instance, "stop_service"):
                instance.stop_service()
            del self._retiring[pid]

    def __stop_plugins(self):
        for pid, lock in list(self._locks.items()):
            with lock:
                if pid in self._running_plugins:
                    self._retiring[pid] = self._running_plugins.pop(pid)
                self._finish_stop(pid)

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
        return self._call_instance(pid, "get_page") or (None, None, None)

    def get_plugin_script(self, pid):
        """
        获取插件额外脚本
        """
        return self._call_instance(pid, "get_script")

    def get_plugin_state(self, pid):
        """
        获取插件状态
        """
        return self._call_instance(pid, "get_state")

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

    def _ordered_instances(self):
        instances = self._running_plugins.copy()
        return [(pid, instances[pid]) for pid in self._plugins if pid in instances]

    def get_plugins_conf(self, auth_level, selected=None):
        visible = [(pid, plugin, getattr(plugin, "auth_level", 0))
                   for pid, plugin in self._ordered_instances()
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
            with self._locks[pid]:
                if self._running_plugins.get(pid) is not plugin:
                    continue
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
                with self._locks[pid]:
                    if self._running_plugins.get(pid) is not plugin:
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
        for _, plugin in self._ordered_instances():
            if hasattr(plugin, "get_command"):
                ret_commands.append(plugin.get_command())
        return ret_commands

    def run_plugin_method(self, pid, method, *args, **kwargs):
        """
        运行插件方法
        """
        if self._go_indexer(pid):
            from app.indexer.indexerConf import plugin_indexers, plugin_search
            if method == "get_indexers":
                return plugin_indexers(pid)
            if method == "search":
                return plugin_search(pid, *args, **kwargs)
            return None
        return self._call_instance(pid, method, *args, **kwargs)
