import os
import time

from watchdog.events import FileSystemEventHandler
from watchdog.observers import Observer
from werkzeug.security import generate_password_hash

import log
from app.helper import ProgressHelper
from app.plugins import PluginManager
from app.media import Category
from app.utils import ConfigLoadCache, CategoryLoadCache, ExceptionUtils, StringUtils
from app.utils.commons import INSTANCES
from app.utils.types import BuiltinIndexerFileMd5
from config import Config
from web.action import WebAction

_observer = Observer(timeout=10)


def check_config():
    """
    检查配置文件，如有错误进行日志输出
    """
    # 检查日志输出
    if Config().get_config('app'):
        logtype = Config().get_config('app').get('logtype')
        if logtype:
            log.info(f"日志输出类型为：{logtype}")
        if logtype == "server":
            logserver = Config().get_config('app').get('logserver')
            if not logserver:
                log.warn("【Config】日志中心地址未配置，无法正常输出日志")
            else:
                log.info("日志将上送到服务器：{logserver}")

        # 检查WEB端口
        web_port = Config().get_config('app').get('web_port')
        if not web_port:
            log.warn("【Config】WEB服务端口未设置，将使用默认3000端口")

        # 检查登录用户和密码
        login_user = Config().get_config('app').get('login_user')
        login_password = Config().get_config('app').get('login_password')
        if not login_user or not login_password:
            log.warn("【Config】WEB管理用户或密码未设置，将使用默认用户：admin，密码：password")
        else:
            log.info(f"WEB管理页面用户：{str(login_user)}")

        # 检查HTTPS
        ssl_cert = Config().get_config('app').get('ssl_cert')
        ssl_key = Config().get_config('app').get('ssl_key')
        if not ssl_cert or not ssl_key:
            log.info(f"未启用https，请使用 http://IP:{str(web_port)} 访问管理页面")
        else:
            if not os.path.exists(ssl_cert):
                log.warn(f"【Config】ssl_cert文件不存在：{ssl_cert}")
            if not os.path.exists(ssl_key):
                log.warn(f"【Config】ssl_key文件不存在：{ssl_key}")
            log.info(f"已启用https，请使用 https://IP:{str(web_port)} 访问管理页面")
    else:
        log.error("【Config】配置文件格式错误，找不到app配置项！")

    builtin_indexer_path = Config().get_builtin_indexer_path()
    builtin_indexer_verify_result = StringUtils.verify_integrity(builtin_indexer_path, BuiltinIndexerFileMd5)
    if not builtin_indexer_verify_result:
        recovery_msg = """
------------------------------------------------------------------
【Config】内置索引文件被改动，为保证稳定性，请检查是否安装第三方插件或者人为修改
1. 如果为docker容器/套件，请删除容器/套件重新添加
2. 如果为其他版本，请重新下载
------------------------------------------------------------------
        """
        log.error(recovery_msg)


def update_config():
    """
    升级配置文件
    """
    _config = Config().get_config()
    overwrite_cofig = False

    # 密码初始化
    login_password = _config.get("app", {}).get("login_password") or "password"
    if login_password and not login_password.startswith("[hash]"):
        _config['app']['login_password'] = "[hash]%s" % generate_password_hash(
            login_password)
        overwrite_cofig = True

    # API密钥初始化
    if not _config.get("security", {}).get("api_key"):
        _config['security']['api_key'] = StringUtils.generate_random_str(32)
        overwrite_cofig = True

    # 站点数据刷新时间默认配置
    try:
        if "ptrefresh_date_cron" not in _config['pt']:
            _config['pt']['ptrefresh_date_cron'] = '6'
            overwrite_cofig = True
    except Exception as e:
        ExceptionUtils.exception_traceback(e)

    # 站点签到转为插件
    try:
        ptsignin_cron = Config().get_config("pt").get("ptsignin_cron")
        if ptsignin_cron:
            # 转换周期
            ptsignin_cron = str(ptsignin_cron).strip()
            if ptsignin_cron.isdigit():
                cron = f"0 */{ptsignin_cron} * * *"
            elif ptsignin_cron.count(" ") == 4:
                cron = ptsignin_cron
            elif "-" in ptsignin_cron:
                ptsignin_cron = ptsignin_cron.split("-")[0]
                hour = int(ptsignin_cron.split(":")[0])
                minute = int(ptsignin_cron.split(":")[1])
                cron = f"{minute} {hour} * * *"
            elif ptsignin_cron.count(":"):
                hour = int(ptsignin_cron.split(":")[0])
                minute = int(ptsignin_cron.split(":")[1])
                cron = f"{minute} {hour} * * *"
            else:
                cron = "30 8 * * *"
            # 安装插件
            WebAction().install_plugin(data={"id": "AutoSignIn"}, reload=False)
            # 保存配置
            PluginManager().save_plugin_config(pid="AutoSignIn", conf={
                "enabled": True,
                "cron": cron,
                "retry_keyword": '',
                "sign_sites": [],
                "special_sites": [],
                "notify": True,
                "onlyonce": False,
                "queue_cnt": 10
            })
            _config['pt'].pop("ptsignin_cron")
            overwrite_cofig = True
    except Exception as e:
        ExceptionUtils.exception_traceback(e)

    # TMDB代理服务开关迁移
    try:
        tmdb_proxy = Config().get_config('laboratory').get("tmdb_proxy")
        if tmdb_proxy:
            _config['app']['tmdb_domain'] = 'api.tmdb.org'
            _config['laboratory'].pop("tmdb_proxy")
            overwrite_cofig = True
    except Exception as e:
        ExceptionUtils.exception_traceback(e)

    # baiduocr配置文件迁移
    try:
        ocr = Config().get_config('ocr')
        if not ocr:
            _config['ocr'] = {}
            _config['ocr']['custom_ocr_url'] = ''
            _config['ocr']['baiduocr_api_key'] = ''
            _config['ocr']['baiduocr_secret_key'] = ''
            overwrite_cofig = True
        baidu_ocr = Config().get_config('baiduocr')
        if baidu_ocr:
            _config['ocr']['baiduocr_api_key'] = baidu_ocr.get('api_key', '') or ''
            _config['ocr']['baiduocr_secret_key'] = baidu_ocr.get('secret_key', '') or ''
            _config.pop("baiduocr")
            overwrite_cofig = True
    except Exception as e:
        ExceptionUtils.exception_traceback(e)

    # 重写配置文件
    if overwrite_cofig:
        Config().save_config(_config)


class ConfigMonitor(FileSystemEventHandler):
    """
    配置文件变化响应
    """

    def __init__(self):
        FileSystemEventHandler.__init__(self)

    def on_modified(self, event):
        if event.is_directory:
            return
        src_path = event.src_path
        file_name = os.path.basename(src_path)
        file_head, file_ext = os.path.splitext(os.path.basename(file_name))
        if file_ext != ".yaml":
            return
        # 配置文件10秒内只能加载一次
        if file_name == "config.yaml" and not ConfigLoadCache.get(src_path):
            ConfigLoadCache.set(src_path, True)
            CategoryLoadCache.set("ConfigLoadBlock", True, ConfigLoadCache.ttl)
            log.warn(f"【System】进程 {os.getpid()} 检测到系统配置文件已修改，正在重新加载...")
            time.sleep(1)
            # 重新加载配置
            Config().init_config()
            # 重载singleton服务
            for instance in INSTANCES.values():
                if hasattr(instance, "init_config"):
                    instance.init_config()

            # 重新应用日志等级
            log.refresh_loglevel()
        # 正在使用的二级分类策略文件3秒内只能加载一次，配置文件加载时，二级分类策略文件不加载
        elif file_name == os.path.basename(Config().category_path) \
                and not CategoryLoadCache.get(src_path) \
                and not CategoryLoadCache.get("ConfigLoadBlock"):
            CategoryLoadCache.set(src_path, True)
            log.warn(f"【System】进程 {os.getpid()} 检测到二级分类策略 {file_head} 配置文件已修改，正在重新加载...")
            time.sleep(1)
            # 重新加载二级分类策略
            Category().init_config()


def start_config_monitor():
    """
    启动服务
    """
    global _observer
    # 配置文件监听
    _observer.schedule(ConfigMonitor(), path=Config().get_config_path(), recursive=False)
    _observer.daemon = True
    _observer.start()


def stop_config_monitor():
    """
    停止服务
    """
    global _observer
    try:
        if _observer:
            _observer.stop()
            _observer.join()
    except Exception as err:
        print(str(err))
