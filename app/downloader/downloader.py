import os
import base64
import hashlib
from threading import Lock
from enum import Enum
import json
import subprocess

from apscheduler.schedulers.background import BackgroundScheduler

import log
from app.conf import ModuleConf
from app.conf import SystemConfig
from app.downloader.client._base import _IDownloadClient
from app.filetransfer import FileTransfer
from app.helper.db_helper import download_history_media
from app.helper import DbHelper, ThreadHelper, SubmoduleHelper
from app.media import Media
from app.mediaserver import MediaServer
from app.message import Message
from app.plugins import EventManager
from app.sites import Sites, SiteSubtitle
from app.utils import Torrent, StringUtils, SystemUtils, ExceptionUtils, NumberUtils
from app.utils.commons import singleton
from app.utils.types import MediaType, DownloaderType, SearchType, RmtMode, EventType, SystemConfigKey
from config import Config, go_helper_path, run_go_helper, PT_TAG, PT_TRANSFER_INTERVAL

lock = Lock()
client_lock = Lock()


@singleton
class Downloader:
    # 客户端实例
    clients = {}

    _downloader_schema = []
    _download_order = None
    _download_settings = {}
    _downloader_confs = {}
    _monitor_downloader_ids = []
    # 下载器ID-名称枚举类
    _DownloaderEnum = None
    _scheduler = None

    message = None
    mediaserver = None
    media = None
    filetransfer = None
    sites = None
    sitesubtitle = None
    dbhelper = None
    systemconfig = None
    eventmanager = None

    def __init__(self):
        self._downloader_schema = SubmoduleHelper.import_submodules(
            'app.downloader.client',
            filter_func=lambda _, obj: hasattr(obj, 'client_id')
        )
        log.debug(f"【Downloader】加载下载器类型：{self._downloader_schema}")
        self.init_config()

    def init_config(self):
        self.dbhelper = DbHelper()
        self.message = Message()
        self.mediaserver = MediaServer()
        self.media = Media()
        self.filetransfer = FileTransfer()
        self.sites = Sites()
        self.systemconfig = SystemConfig()
        self.eventmanager = EventManager()
        self.sitesubtitle = SiteSubtitle()
        # 清空已存在下载器实例
        self.clients = {}
        # 下载器配置，生成实例
        self._downloader_confs = {}
        self._monitor_downloader_ids = []
        for downloader_conf in self.dbhelper.get_downloaders():
            if not downloader_conf:
                continue
            did = downloader_conf.ID
            name = downloader_conf.NAME
            enabled = downloader_conf.ENABLED
            # 下载器监控配置
            transfer = downloader_conf.TRANSFER
            only_nastool = downloader_conf.ONLY_NASTOOL
            match_path = downloader_conf.MATCH_PATH
            rmt_mode = downloader_conf.RMT_MODE
            rmt_mode_name = ModuleConf.RMT_MODES.get(rmt_mode).value if rmt_mode else ""
            # 输出日志
            if transfer:
                log_content = ""
                if only_nastool:
                    log_content += "启用标签隔离，"
                if match_path:
                    log_content += "启用目录隔离，"
                log.info(f"【Downloader】读取到监控下载器：{name}{log_content}转移方式：{rmt_mode_name}")
                if enabled:
                    self._monitor_downloader_ids.append(did)
                else:
                    log.info(f"【Downloader】下载器：{name} 不进行监控：下载器未启用")
            # 下载器登录配置
            config = json.loads(downloader_conf.CONFIG)
            dtype = downloader_conf.TYPE
            self._downloader_confs[str(did)] = {
                "id": did,
                "name": name,
                "type": dtype,
                "enabled": enabled,
                "transfer": transfer,
                "only_nastool": only_nastool,
                "match_path": match_path,
                "rmt_mode": rmt_mode,
                "rmt_mode_name": rmt_mode_name,
                "config": config,
                "download_dir": json.loads(downloader_conf.DOWNLOAD_DIR)
            }
        # 下载器ID-名称枚举类生成
        self._DownloaderEnum = Enum('DownloaderIdName',
                                    {did: conf.get("name") for did, conf in self._downloader_confs.items()})
        pt = Config().get_config('pt')
        if pt:
            self._download_order = pt.get("download_order")
        # 下载设置
        self._download_settings = {
            "-1": {
                "id": -1,
                "name": "预设",
                "category": '',
                "tags": PT_TAG,
                "is_paused": 0,
                "upload_limit": 0,
                "download_limit": 0,
                "ratio_limit": 0,
                "seeding_time_limit": 0,
                "downloader": "",
                "downloader_name": "",
                "downloader_type": ""
            }
        }
        download_settings = self.dbhelper.get_download_setting()
        for download_setting in download_settings:
            downloader_id = download_setting.DOWNLOADER
            download_conf = self._downloader_confs.get(str(downloader_id))
            if download_conf:
                downloader_name = download_conf.get("name")
                downloader_type = download_conf.get("type")
            else:
                downloader_name = ""
                downloader_type = ""
                downloader_id = ""
            self._download_settings[str(download_setting.ID)] = {
                "id": download_setting.ID,
                "name": download_setting.NAME,
                "category": download_setting.CATEGORY,
                "tags": download_setting.TAGS,
                "is_paused": download_setting.IS_PAUSED,
                "upload_limit": download_setting.UPLOAD_LIMIT,
                "download_limit": download_setting.DOWNLOAD_LIMIT,
                "ratio_limit": download_setting.RATIO_LIMIT / 100,
                "seeding_time_limit": download_setting.SEEDING_TIME_LIMIT,
                "downloader": downloader_id,
                "downloader_name": downloader_name,
                "downloader_type": downloader_type
            }
        # 启动下载器监控服务
        self.start_service()

    def __build_class(self, ctype, conf=None):
        for downloader_schema in self._downloader_schema:
            try:
                if downloader_schema.match(ctype):
                    return downloader_schema(conf)
            except Exception as e:
                ExceptionUtils.exception_traceback(e)
        return None

    @property
    def default_downloader_id(self):
        """
        获取默认下载器id
        """
        default_downloader_id = SystemConfig().get(SystemConfigKey.DefaultDownloader)
        if not default_downloader_id or not self.get_downloader_conf(default_downloader_id):
            default_downloader_id = ""
        return default_downloader_id

    @property
    def default_download_setting_id(self):
        """
        获取默认下载设置
        :return: 默认下载设置id
        """
        default_download_setting_id = SystemConfig().get(SystemConfigKey.DefaultDownloadSetting) or "-1"
        if not self._download_settings.get(default_download_setting_id):
            default_download_setting_id = "-1"
        return default_download_setting_id

    def get_downloader_type(self, downloader_id=None):
        """
        获取下载器的类型
        """
        if not downloader_id:
            return self.default_client.get_type()
        return self.__get_client(downloader_id).get_type()

    @property
    def default_client(self):
        """
        获取默认下载器实例
        """
        return self.__get_client(self.default_downloader_id)

    @property
    def monitor_downloader_ids(self):
        """
        获取监控下载器ID列表
        """
        return self._monitor_downloader_ids

    def start_service(self):
        """
        转移任务调度
        """
        # 移出现有任务
        self.stop_service()
        # 启动转移任务
        if not self._monitor_downloader_ids:
            return
        self._scheduler = BackgroundScheduler(timezone=Config().get_timezone())
        for downloader_id in self._monitor_downloader_ids:
            self._scheduler.add_job(func=self.transfer,
                                    args=[downloader_id],
                                    trigger='interval',
                                    seconds=PT_TRANSFER_INTERVAL)
        self._scheduler.print_jobs()
        self._scheduler.start()
        log.info("下载文件转移服务启动，目的目录：媒体库")

    def __get_client(self, did=None):
        if not did:
            return None
        downloader_conf = self.get_downloader_conf(did)
        if not downloader_conf:
            log.info("【Downloader】下载器配置不存在")
            return None
        if not downloader_conf.get("enabled"):
            log.info(f"【Downloader】下载器 {downloader_conf.get('name')} 未启用")
            return None
        ctype = downloader_conf.get("type")
        config = dict(downloader_conf.get("config") or {})
        config["download_dir"] = downloader_conf.get("download_dir")
        config["name"] = downloader_conf.get("name")
        with client_lock:
            if not self.clients.get(str(did)):
                self.clients[str(did)] = self.__build_class(ctype, config)
            return self.clients.get(str(did))

    def download(self,
                 media_info,
                 is_paused=None,
                 tag=None,
                 download_dir=None,
                 download_setting=None,
                 downloader_id=None,
                 upload_limit=None,
                 download_limit=None,
                 torrent_file=None,
                 in_from=None,
                 user_name=None,
                 skip_size_check=False,
                 proxy=None,
                 receipt_sender=None,
                 batch=None,
                 rss_article=None):
        """
        添加下载任务，根据当前使用的下载器分别调用不同的客户端处理
        :param media_info: 需下载的媒体信息，含URL地址
        :param is_paused: 是否暂停下载
        :param tag: 种子标签
        :param download_dir: 指定下载目录
        :param download_setting: 下载设置id，为None则使用-1默认设置，为"-2"则不使用下载设置
        :param downloader_id: 指定下载器ID下载
        :param upload_limit: 上传速度限制
        :param download_limit: 下载速度限制
        :param torrent_file: 种子文件路径
        :param in_from: 来源
        :param user_name: 用户名
        :param skip_size_check: 跳过尺寸检查，用于刷流部分下载
        :param proxy: 是否使用代理，指定该选项为 True/False 会覆盖 site_info 的设置
        :return: 下载器类型, 种子ID，错误信息
        """

        def __download_fail(msg):
            """
            触发下载失败事件和发送消息
            """
            self.eventmanager.send_event(EventType.DownloadFail, {
                "media_info": media_info.to_dict(),
                "reason": msg
            })
            if in_from:
                self.message.send_download_fail_message(media_info, f"添加下载任务失败：{msg}")

        # 触发下载事件
        self.eventmanager.send_event(EventType.DownloadAdd, {
            "media_info": media_info.to_dict(),
            "is_paused": is_paused,
            "tag": tag,
            "download_dir": download_dir,
            "download_setting": download_setting,
            "downloader_id": downloader_id,
            "torrent_file": torrent_file
        })

        # 标题
        title = media_info.org_string
        # 详情页面
        page_url = media_info.page_url
        # 默认值
        site_info, dl_files_folder, dl_files, retmsg = {}, "", [], ""
        container_path = download_dir
        if torrent_file:
            # 有种子文件时解析种子信息
            url = os.path.basename(torrent_file)
            content, dl_files_folder, dl_files, retmsg = Torrent().read_torrent_content(torrent_file)
        else:
            # 没有种子文件解析链接
            url = media_info.enclosure
            if not url:
                __download_fail("下载链接为空")
                return None, None, None, "下载链接为空"
            # 获取种子内容，磁力链不解析
            if url.startswith("magnet:"):
                content = url
            else:
                # 获取Cookie和ua等
                site_info = self.sites.get_sites(siteurl=url)
                # 下载种子文件，并读取信息
                _, content, dl_files_folder, dl_files, retmsg = Torrent().get_torrent_info(
                    url=url,
                    cookie=site_info.get("cookie"),
                    ua=site_info.get("ua"),
                    referer=page_url if site_info.get("referer") else None,
                    proxy=proxy if proxy is not None else site_info.get("proxy")
                )

        # 解析完成
        if retmsg:
            log.warn("【Downloader】%s" % retmsg)

        if not content:
            __download_fail(retmsg)
            return None, None, None, retmsg

        if batch and batch.get("digest"):
            if not isinstance(content, bytes) or hashlib.sha256(content).digest() != batch["digest"]:
                return None, None, None, "种子文件已变更，缺集保留"

        # 下载设置
        if not download_setting and media_info.site:
            # 站点的下载设置
            download_setting = self.sites.get_site_download_setting(media_info.site)
        if download_setting == "-2":
            # 不使用下载设置
            download_attr = {}
        elif download_setting:
            # 传入的下载设置
            download_attr = self.get_download_setting(download_setting) \
                            or self.get_download_setting(self.default_download_setting_id)
        else:
            # 默认下载设置
            download_attr = self.get_download_setting(self.default_download_setting_id)

        # 下载设置名称
        download_setting_name = download_attr.get('name')

        # 下载器实例
        if not downloader_id:
            downloader_id = download_attr.get("downloader")
        downloader_conf = self.get_downloader_conf(downloader_id)
        sender = receipt_sender or (user_name if in_from == SearchType.WEB else None)
        clients = ("aria2", "qbittorrent", "transmission")
        if rss_article is not None:
            sender = f"userrss:{rss_article['taskId']}"
            clients += ("pan115", "pikpak")
        elif in_from == SearchType.RSS and getattr(media_info, "ordinary_receipt", None) and isinstance(content, str) and Torrent.is_magnet(content):
            clients += ("pan115", "pikpak")
        recorded = bool(sender and downloader_conf and downloader_conf.get("type") in clients)
        if rss_article is not None and not recorded:
            return downloader_id, None, None, "自定义 RSS 下载器不可用"
        if batch and batch.get("partial") and not (recorded and downloader_conf["type"] in ("qbittorrent", "transmission")):
            return downloader_id, None, None, "当前下载器无法选择剧集文件"
        downloader = None if recorded else self.__get_client(downloader_id)

        if not downloader_conf or (not recorded and not downloader):
            __download_fail("请检查下载设置所选下载器是否有效且启用")
            return None, None, None, f"下载设置 {download_setting_name} 所选下载器失效"
        downloader_name = downloader_conf.get("name")

        # 开始添加下载
        try:
            # 下载设置中的分类
            category = download_attr.get("category")
            # 合并TAG
            tags = download_attr.get("tags")
            if tags:
                tags = str(tags).split(";")
                if tag:
                    if isinstance(tag, list):
                        tags.extend(tag)
                    else:
                        tags.append(tag)
            else:
                # 字符串是空串或者None
                tags = []
                if tag:
                    if isinstance(tag, list):
                        tags = list(tag)
                    else:
                        tags = [tag]
            # 添加站点tag
            site_tags = self.sites.get_site_download_tags(media_info.site)
            if site_tags:
                tags.extend(str(site_tags).split(";"))

            # 暂停
            if is_paused is None:
                is_paused = StringUtils.to_bool(download_attr.get("is_paused"))
            else:
                is_paused = True if is_paused else False
            # 上传限速
            if not upload_limit:
                upload_limit = download_attr.get("upload_limit")
            # 下载限速
            if not download_limit:
                download_limit = download_attr.get("download_limit")
            # 分享率
            ratio_limit = download_attr.get("ratio_limit")
            # 做种时间
            seeding_time_limit = download_attr.get("seeding_time_limit")
            # 下载目录设置
            if not download_dir:
                log.info(f"【Downloader】下载器 before, down_dir: {download_dir}, media: {media_info}, down_conf: {downloader_conf}")
                download_info = self.__get_download_dir_info(media_info, downloader_conf.get("download_dir"), skip_size_check)
                download_dir = download_info.get('path')
                container_path = download_info.get('container_path')
                # 从下载目录中获取分类标签
                log.info(f"【Downloader】下载器 after, down_dir: {download_dir}")
                if not category:
                    category = download_info.get('category')
            # 计算数据文件保存的路径
            save_dir = subtitle_dir = None
            visit_dir, _ = _IDownloadClient.get_replace_path(download_dir, downloader_conf.get("download_dir"))
            if visit_dir:
                if dl_files_folder:
                    # 种子文件带目录
                    save_dir = os.path.join(visit_dir, dl_files_folder)
                    subtitle_dir = save_dir
                elif dl_files:
                    # 种子文件为单独文件
                    save_dir = os.path.join(visit_dir, dl_files[0])
                    subtitle_dir = visit_dir
                else:
                    save_dir = None
                    subtitle_dir = visit_dir
            # 添加下载
            print_url = "磁力链接" if isinstance(content, str) else (os.path.basename(torrent_file) if torrent_file else "站点种子")
            if is_paused:
                log.info(f"【Downloader】下载器 {downloader_name} 添加任务并暂停：%s，目录：%s，Url：%s" % (
                    title, download_dir, print_url))
            else:
                log.info(f"【Downloader】下载器 {downloader_name} 添加任务：%s，目录：%s，Url：%s" % (
                    title, download_dir, print_url))
            # 下载ID
            download_id = None
            if batch is not None:
                batch["pending"] = True
            if recorded:
                try:
                    receipt = self.__submit_recorded_download(
                        media_info, content, in_from, sender, user_name, downloader_conf, download_setting_name,
                        download_dir, save_dir, is_paused, upload_limit, download_limit,
                        tags, category, ratio_limit, seeding_time_limit, batch.get("proof") if batch else None, rss_article)
                except (OSError, subprocess.SubprocessError, ValueError):
                    return downloader_id, None, None, "无法确认下载提交结果，请检查下载回执"
                if rss_article is not None and receipt.get("processed"):
                    rss_article["processed"] = True
                    return downloader_id, None, None, "资源已处理"
                if receipt.get("rejected"):
                    if batch is not None:
                        batch["pending"] = False
                    reason = "未添加下载，请核对下载器设置及是否已存在相同任务"
                    __download_fail(reason)
                    return downloader_id, None, None, reason
                if receipt.get("pending"):
                    return downloader_id, None, None, "已有下载回执待核对，未重复提交"
                if batch is not None:
                    batch["pending"] = False
                if receipt.get("subscriptionRecorded"):
                    media_info.subscription_recorded = True
                download_id = receipt["task"]
                if receipt["reused"]:
                    return downloader_id, download_id, container_path, f"已提交过此下载，原任务编号：{download_id}"
                ret = bool(download_id)
            elif downloader.get_type() == DownloaderType.TR:
                ret = downloader.add_torrent(content,
                                             is_paused=is_paused,
                                             download_dir=download_dir,
                                             cookie=site_info.get("cookie"))
                if ret:
                    download_id = ret.hashString
                    downloader.change_torrent(tid=download_id,
                                              tag=tags,
                                              upload_limit=upload_limit,
                                              download_limit=download_limit,
                                              ratio_limit=ratio_limit,
                                              seeding_time_limit=seeding_time_limit)

            elif downloader.get_type() == DownloaderType.QB:
                # 加标签以获取添加下载后的编号
                torrent_tag = "NT" + StringUtils.generate_random_str(5)
                if tags:
                    tags += [torrent_tag]
                else:
                    tags = [torrent_tag]
                # 布局默认原始
                ret = downloader.add_torrent(content,
                                             is_paused=is_paused,
                                             download_dir=download_dir,
                                             tag=tags,
                                             category=category,
                                             content_layout="Original",
                                             upload_limit=upload_limit,
                                             download_limit=download_limit,
                                             ratio_limit=ratio_limit,
                                             seeding_time_limit=seeding_time_limit,
                                             cookie=site_info.get("cookie"))
                if ret:
                    download_id = downloader.get_torrent_id_by_tag(torrent_tag)
            else:
                # 其它下载器，添加下载后需返回下载ID或添加状态
                ret = downloader.add_torrent(content,
                                             is_paused=is_paused,
                                             tag=tags,
                                             download_dir=download_dir,
                                             category=category)
                download_id = ret
            if ret and (not download_id or isinstance(download_id, bool)):
                return downloader_id, None, None, "下载器已响应，任务编号尚未确认，请先核对原任务"
            # 添加下载成功
            if ret:
                # 登记下载历史，记录下载目录
                warning = ""
                if not recorded:
                    try:
                        self.dbhelper.insert_download_history(media_info=media_info, downloader=downloader_id,
                                                              download_id=download_id, save_dir=save_dir)
                    except (OSError, ValueError, subprocess.SubprocessError):
                        warning = "下载器已接收任务，历史记录尚未保存"
                        log.warn(f"【Downloader】{warning}")
                # 下载站点字幕文件
                if page_url \
                        and subtitle_dir \
                        and site_info \
                        and site_info.get("subtitle"):
                    try:
                        ThreadHelper().start_thread(
                            self.sitesubtitle.download,
                            (
                                media_info,
                                site_info.get("id"),
                                site_info.get("cookie"),
                                site_info.get("ua"),
                                subtitle_dir
                            )
                        )
                    except Exception as err:
                        log.warn(f"【Downloader】下载器已接收任务，启动字幕任务失败：{err}")
                # 发送下载消息
                if in_from and not recorded:
                    media_info.user_name = user_name
                    try:
                        self.message.send_download_message(in_from=in_from, can_item=media_info,
                                                           download_setting_name=download_setting_name,
                                                           downloader_name=downloader_name)
                    except (OSError, ValueError, subprocess.SubprocessError):
                        warning = (warning + "；" if warning else "") + "下载器已接收任务，通知尚未确认"
                        log.warn(f"【Downloader】{warning}")
                return downloader_id, download_id, container_path, warning
            else:
                __download_fail("请检查下载任务是否已存在")
                return downloader_id, None, None, f"下载器 {downloader_name} 添加下载任务失败，请检查下载任务是否已存在"
        except Exception as e:
            ExceptionUtils.exception_traceback(e)
            __download_fail(str(e))
            log.error(f"【Downloader】下载器 {downloader_name} 添加任务出错：%s" % str(e))
            return None, None, None, str(e)

    def __submit_recorded_download(self, media_info, content, in_from, sender, user_name, downloader_conf,
                                   download_setting_name, download_dir, save_dir, is_paused, upload_limit, download_limit,
                                   tags, category, ratio_limit, seeding_time_limit, proof=None, rss_article=None):
        media_info.user_name = user_name
        notice = self.message.download_notice_fields(in_from, media_info, download_setting_name, downloader_conf["name"])
        domain = Config().get_domain()
        request = {
            "downloader": int(downloader_conf["id"]), "clientType": downloader_conf["type"], "config": downloader_conf["config"],
            "directories": downloader_conf["download_dir"], "sender": str(sender), "channel": in_from.name,
            "source": in_from.value, "title": media_info.org_string, "directory": download_dir or "", "paused": is_paused,
            "uploadLimitKB": int(upload_limit or 0), "downloadLimitKB": int(download_limit or 0), "notice": notice,
            "history": {"media": download_history_media(media_info), "path": save_dir,
                        "image": media_info.get_message_image() or "", "link": domain + "?next=downloading" if domain else ""},
        }
        if rss_article is not None:
            request["history"]["rss"] = rss_article
        elif in_from == SearchType.RSS and getattr(media_info, "ordinary_receipt", None):
            request["history"]["subscription"] = media_info.ordinary_receipt
        if proof:
            request["batch"] = proof
        if downloader_conf["type"] in ("qbittorrent", "transmission"):
            request.update(tags=tags, ratioLimit=round(float(ratio_limit or 0), 2),
                           seedingTimeLimit=int(seeding_time_limit or 0))
        if downloader_conf["type"] == "qbittorrent":
            request["category"] = category or ""
        if isinstance(content, str):
            request["magnet"] = content
        else:
            request["torrent"] = base64.b64encode(content).decode("ascii")
        return json.loads(run_go_helper("classic-download", "submit", payload=json.dumps(request).encode(), timeout=75))

    def transfer(self, downloader_id=None):
        """
        转移下载完成的文件，进行文件识别重命名到媒体库目录
        """
        downloader_ids = [downloader_id] if downloader_id \
            else self._monitor_downloader_ids
        for downloader_id in downloader_ids:
            with lock:
                # 获取下载器配置
                downloader_conf = self.get_downloader_conf(downloader_id)
                name = downloader_conf.get("name")
                only_nastool = downloader_conf.get("only_nastool")
                match_path = downloader_conf.get("match_path")
                rmt_mode = ModuleConf.RMT_MODES.get(downloader_conf.get("rmt_mode"))
                # 获取下载器实例
                _client = self.__get_client(downloader_id)
                if not _client:
                    continue
                trans_tasks = _client.get_transfer_task(tag=PT_TAG if only_nastool else None, match_path=match_path)
                if trans_tasks:
                    log.info(f"【Downloader】下载器 {name} 开始转移下载文件...")
                else:
                    continue
                for task in trans_tasks:
                    done_flag, done_msg = self.filetransfer.transfer_media(
                        in_from=self._DownloaderEnum[str(downloader_id)],
                        in_path=task.get("path"),
                        rmt_mode=rmt_mode)
                    if not done_flag:
                        log.warn(f"【Downloader】下载器 {name} 任务%s 转移失败：%s" % (task.get("path"), done_msg))
                        _client.set_torrents_status(ids=task.get("id"),
                                                    tags=task.get("tags"))
                    else:
                        if rmt_mode in [RmtMode.MOVE, RmtMode.RCLONE, RmtMode.MINIO]:
                            log.warn(f"【Downloader】下载器 {name} 移动模式下删除种子文件：%s" % task.get("id"))
                            _client.delete_torrents(delete_file=True, ids=task.get("id"))
                        else:
                            _client.set_torrents_status(ids=task.get("id"),
                                                        tags=task.get("tags"))
                log.info(f"【Downloader】下载器 {name} 下载文件转移结束")

    def get_torrents(self, downloader_id=None, ids=None, tag=None):
        """
        获取种子信息
        :param downloader_id: 下载器ID
        :param ids: 种子ID
        :param tag: 种子标签
        :return: 种子信息列表
        """
        if not downloader_id:
            downloader_id = self.default_downloader_id
        _client = self.__get_client(downloader_id)
        if not _client:
            log.error(f"【Downloader】为获取到对应的 client, {downloader_id}")
            return None
        try:
            torrents, error_flag = _client.get_torrents(tag=tag, ids=ids)
            if error_flag:
                log.error(f"【Downloader】获取种子列表异常 {tag}, ${ids}")
                return None
            return torrents
        except Exception as err:
            ExceptionUtils.exception_traceback(err)
            return None

    def get_remove_torrents(self, downloader_id=None, config=None):
        """
        查询符合删种策略的种子信息
        :return: 符合删种策略的种子信息列表
        """
        if not config or not downloader_id:
            return []
        _client = self.__get_client(downloader_id)
        if not _client:
            return []
        config["filter_tags"] = []
        if config.get("onlynastool"):
            config["filter_tags"] = config["tags"] + [PT_TAG]
        else:
            config["filter_tags"] = config["tags"]
        torrents = _client.get_remove_torrents(config=config)
        torrents.sort(key=lambda x: x.get("name"))
        return torrents

    def get_downloading_torrents(self, downloader_id=None, ids=None, tag=None):
        """
        查询正在下载中的种子信息
        :return: 下载器名称，发生错误时返回None
        """
        if not downloader_id:
            downloader_id = self.default_downloader_id
        _client = self.__get_client(downloader_id)
        if not _client:
            return None
        try:
            return _client.get_downloading_torrents(tag=tag, ids=ids)
        except Exception as err:
            ExceptionUtils.exception_traceback(err)
            return None

    def get_downloading_progress(self, downloader_id=None, ids=None, force_list = False):
        """
        查询正在下载中的进度信息
        """
        if not downloader_id:
            downloader_id = self.default_downloader_id
        downloader_conf = self.get_downloader_conf(downloader_id)
        only_nastool = downloader_conf.get("only_nastool") if not force_list else False
        _client = self.__get_client(downloader_id)
        if not _client:
            return []
        if only_nastool:
            tag = [PT_TAG]
        else:
            tag = None
        return _client.get_downloading_progress(tag=tag, ids=ids)

    def get_completed_torrents(self, downloader_id=None, ids=None, tag=None):
        """
        查询下载完成的种子列表
        :param downloader_id: 下载器ID
        :param ids: 种子ID列表
        :param tag: 种子标签
        :return: 种子信息列表，发生错误时返回None
        """
        if not downloader_id:
            downloader_id = self.default_downloader_id
        _client = self.__get_client(downloader_id)
        if not _client:
            return None
        return _client.get_completed_torrents(ids=ids, tag=tag)

    def start_torrents(self, downloader_id=None, ids=None):
        """
        下载控制：开始
        :param downloader_id: 下载器ID
        :param ids: 种子ID列表
        :return: 处理状态
        """
        if not ids:
            return False
        _client = self.__get_client(downloader_id) if downloader_id else self.default_client
        if not _client:
            return False
        return _client.start_torrents(ids)

    def stop_torrents(self, downloader_id=None, ids=None):
        """
        下载控制：停止
        :param downloader_id: 下载器ID
        :param ids: 种子ID列表
        :return: 处理状态
        """
        if not ids:
            return False
        _client = self.__get_client(downloader_id) if downloader_id else self.default_client
        if not _client:
            return False
        return _client.stop_torrents(ids)

    def delete_torrents(self, downloader_id=None, ids=None, delete_file=False):
        """
        删除种子
        :param downloader_id: 下载器ID
        :param ids: 种子ID列表
        :param delete_file: 是否删除文件
        :return: 处理状态
        """
        if not ids:
            return False
        _client = self.__get_client(downloader_id) if downloader_id else self.default_client
        if not _client:
            return False
        return _client.delete_torrents(delete_file=delete_file, ids=ids)

    def batch_download(self, in_from, media_list, need_tvs=None, user_name=None, receipt_sender=None):
        """Go owns ranking and missing-episode progress. Legacy objects stay with their caller."""
        if not media_list:
            return [], need_tvs

        original_keys = {str(key): key for key in need_tvs or {}}
        candidates = []
        for item in media_list:
            candidates.append({
                "show": str(item.tmdb_id), "title": str(item.title), "display": item.get_title_string(),
                "label": item.get_season_episode_string(), "movie": item.type == MediaType.MOVIE,
                "seasons": item.get_season_list(), "episodes": item.get_episode_list(),
                "resource": str(item.res_order), "site": str(item.site_order), "seeders": str(item.seeders),
            })
        query = {"candidates": candidates, "missing": need_tvs or {}, "order": self._download_order or ""}
        inspected = {}
        digests = {}
        with subprocess.Popen([go_helper_path(), "batch-download"], stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as planner:
            def reply(value):
                planner.stdin.write(json.dumps(value, ensure_ascii=False).encode() + b"\n")
                planner.stdin.flush()

            try:
                reply(query)
                while True:
                    action = json.loads(planner.stdout.readline())
                    if action["op"] == "done":
                        if planner.wait() != 0:
                            raise ValueError("批量下载回执异常")
                        accepted = [media_list[i] for i in action["accepted"]]
                        if need_tvs is not None:
                            left = {original_keys[key]: rows for key, rows in action["missing"].items()}
                            need_tvs.clear()
                            need_tvs.update(left)
                        return accepted, need_tvs

                    i = action["index"]
                    item = media_list[i]
                    if action["op"] == "inspect":
                        if i not in inspected:
                            site = self.sites.get_sites(siteurl=item.enclosure) or {}
                            path, content, _ = Torrent().save_torrent_file(
                                item.enclosure, cookie=site.get("cookie"), ua=site.get("ua"),
                                referer=item.page_url if site.get("referer") else None, proxy=site.get("proxy"))
                            if isinstance(content, str) and Torrent.is_magnet(content):
                                path, _ = Torrent.magent2torrent(content, Config().get_temp_path())
                            inspected[i] = path
                        path = inspected[i]
                        try:
                            content = Torrent._read_bytes(path) if path else b""
                        except (OSError, ValueError):
                            content = b""
                        digests[i] = hashlib.sha256(content).digest()
                        reply({"torrent": base64.b64encode(content).decode("ascii")})
                        continue
                    if action["op"] != "submit":
                        raise ValueError("批量下载请求异常")
                    proof = action.get("proof")
                    batch = {"proof": proof, "partial": action.get("partial", False),
                             "digest": digests[i] if proof else None, "pending": False}
                    sender = receipt_sender
                    if not sender and in_from == SearchType.RSS and item.rssid:
                        sender = f"rss:{item.rssid}"
                    if not sender:
                        sender = json.dumps(["batch", in_from.name, user_name or ""])
                    _, task, _, _ = self.download(media_info=item, download_dir=item.save_path,
                                                  download_setting=item.download_setting, in_from=in_from,
                                                  user_name=user_name, receipt_sender=sender, batch=batch,
                                                  torrent_file=inspected.get(i) if proof else None)
                    reply({"accepted": bool(task), "pending": bool(not task and batch["pending"])})
            finally:
                planner.stdin.close()
                if planner.poll() is None:
                    planner.terminate()

    def check_exists_medias(self, meta_info, no_exists=None, total_ep=None):
        """Return coverage, accumulated missing seasons, and display messages."""
        missing = no_exists if no_exists is not None else {}
        try:
            result = self.filetransfer.check_library_inventory(meta_info, total_ep)
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            msg = f"{meta_info.get_title_string()} 暂时无法核实媒体库库存"
            log.warn(f"【Downloader】{msg}")
            return None, missing, [msg]
        for item in result["missing"]:
            seasons = missing.setdefault(meta_info.tmdb_id, [])
            if item not in seasons:
                seasons.append(item)
        return result["complete"], missing, result["messages"]

    def get_files(self, tid, downloader_id=None):
        """
        获取种子文件列表
        """
        # 客户端
        _client = self.__get_client(downloader_id) if downloader_id else self.default_client
        if not _client:
            return []
        # 种子文件
        torrent_files = _client.get_files(tid)
        if not torrent_files:
            return []

        ret_files = []
        if _client.get_type() == DownloaderType.TR:
            for file_id, torrent_file in enumerate(torrent_files):
                ret_files.append({
                    "id": file_id,
                    "name": torrent_file.name,
                    "size": torrent_file.size
                })
        elif _client.get_type() == DownloaderType.QB:
            for torrent_file in torrent_files:
                ret_files.append({
                    "id": torrent_file.get("index"),
                    "name": torrent_file.get("name"),
                    "size": torrent_file.get("size")
                })

        return ret_files

    def get_download_dirs(self, setting=None):
        """
        返回下载器中设置的保存目录
        """
        if not setting:
            setting = self.default_download_setting_id
        # 查询下载设置
        download_setting = self.get_download_setting(sid=setting)
        downloader_conf = self.get_downloader_conf(download_setting.get("downloader"))
        if not downloader_conf:
            return []
        downloaddir = downloader_conf.get("download_dir")
        # 查询目录
        save_path_list = [attr.get("save_path") for attr in downloaddir if attr.get("save_path")]
        save_path_list.sort()
        return list(set(save_path_list))

    def get_download_visit_dirs(self):
        """
        返回所有下载器中设置的访问目录
        """
        download_dirs = []
        for downloader_conf in self.get_downloader_conf().values():
            download_dirs += downloader_conf.get("download_dir")
        visit_path_list = [attr.get("container_path") or attr.get("save_path") for attr in download_dirs if
                           attr.get("save_path")]
        visit_path_list.sort()
        return list(set(visit_path_list))

    def get_download_visit_dir(self, download_dir, downloader_id=None):
        """
        返回下载器中设置的访问目录
        """
        if not downloader_id:
            downloader_id = self.default_downloader_id
        downloader_conf = self.get_downloader_conf(downloader_id)
        _client = self.__get_client(downloader_id)
        if not _client:
            return ""
        true_path, _ = _client.get_replace_path(download_dir, downloader_conf.get("download_dir"))
        return true_path

    @staticmethod
    def __get_download_dir_info(media, downloaddir, skip_size_check=False):
        """
        根据媒体信息读取一个下载目录的信息
        """
        if media:
            for attr in downloaddir or []:
                if not attr:
                    continue
                if attr.get("type") and attr.get("type") != media.type.value:
                    continue
                if attr.get("category") and attr.get("category") != media.category:
                    continue
                if not attr.get("save_path") and not attr.get("label"):
                    continue
                # 刷流的部分下载功能需要跳过目录检查，因为种子文件大小是动态确定的
                if not skip_size_check:
                    if (attr.get("container_path") or attr.get("save_path")) \
                            and os.path.exists(attr.get("container_path") or attr.get("save_path")) \
                            and media.size \
                            and SystemUtils.get_free_space(
                        attr.get("container_path") or attr.get("save_path")
                    ) < NumberUtils.get_size_gb(
                        StringUtils.num_filesize(media.size)
                    ):
                        log.info(f'{attr.get("save_path")} not have enough space')
                        continue
                return {
                    "path": attr.get("save_path"),
                    "category": attr.get("label"),
                    "container_path": attr.get("container_path")
                }
        return {"path": None, "category": None}

    @staticmethod
    def __get_client_type(type_name):
        """
        根据名称返回下载器类型
        """
        if not type_name:
            return None
        for dict_type in DownloaderType:
            if dict_type.name == type_name or dict_type.value == type_name:
                return dict_type

    def get_download_setting(self, sid=None):
        """
        获取下载设置
        :return: 下载设置
        """
        # 更新预设
        preset_downloader_conf = self.get_downloader_conf(self.default_downloader_id)
        if preset_downloader_conf:
            self._download_settings["-1"]["downloader"] = self.default_downloader_id
            self._download_settings["-1"]["downloader_name"] = preset_downloader_conf.get("name")
            self._download_settings["-1"]["downloader_type"] = preset_downloader_conf.get("type")
        if not sid:
            return self._download_settings
        return self._download_settings.get(str(sid)) or {}

    def set_speed_limit(self, downloader_id=None, download_limit=None, upload_limit=None):
        """
        设置速度限制
        :param downloader_id: 下载器ID
        :param download_limit: 下载速度限制，单位KB/s
        :param upload_limit: 上传速度限制，单位kB/s
        """
        if not downloader_id:
            return
        _client = self.__get_client(downloader_id)
        if not _client:
            return
        try:
            download_limit = int(download_limit) if download_limit else 0
        except Exception as err:
            ExceptionUtils.exception_traceback(err)
            download_limit = 0
        try:
            upload_limit = int(upload_limit) if upload_limit else 0
        except Exception as err:
            ExceptionUtils.exception_traceback(err)
            upload_limit = 0
        _client.set_speed_limit(download_limit=download_limit, upload_limit=upload_limit)

    def get_downloader_conf(self, did=None):
        """
        获取下载器配置
        """
        if not did:
            return self._downloader_confs
        return self._downloader_confs.get(str(did))

    def get_downloader_conf_simple(self):
        """
        获取简化下载器配置
        """
        ret_dict = {}
        for downloader_conf in self.get_downloader_conf().values():
            ret_dict[str(downloader_conf.get("id"))] = {
                "id": downloader_conf.get("id"),
                "name": downloader_conf.get("name"),
                "type": downloader_conf.get("type"),
                "enabled": downloader_conf.get("enabled"),
            }
        return ret_dict

    def get_downloader(self, downloader_id=None):
        """
        获取下载器实例
        """
        if not downloader_id:
            return self.default_client
        return self.__get_client(downloader_id)

    def get_downloader(self, downloader_id=None):
        """
        获取下载器实例
        """
        if not downloader_id:
            return self.default_client
        return self.__get_client(downloader_id)

    def get_status(self, dtype=None, config=None):
        """
        测试下载器状态
        """
        if not config or not dtype:
            return False
        # 测试状态
        download_client = self.__build_class(ctype=dtype, conf=config)
        state = download_client.get_status() if download_client else False
        if not state:
            log.error(f"【Downloader】下载器连接测试失败")
        return state

    def recheck_torrents(self, downloader_id=None, ids=None):
        """
        下载控制：重新校验种子
        :param downloader_id: 下载器ID
        :param ids: 种子ID列表
        :return: 处理状态
        """
        if not ids:
            return False
        _client = self.__get_client(downloader_id) if downloader_id else self.default_client
        if not _client:
            return False
        return _client.recheck_torrents(ids)

    def stop_service(self):
        """
        停止服务
        """
        try:
            if self._scheduler:
                self._scheduler.remove_all_jobs()
                if self._scheduler.running:
                    self._scheduler.shutdown()
                self._scheduler = None
        except Exception as e:
            print(str(e))

    def get_download_history(self, date=None, hid=None, num=30, page=1):
        """
        获取下载历史记录
        """
        return self.dbhelper.get_download_history(date=date, hid=hid, num=num, page=page)

    def get_download_history_by_title(self, title):
        """
        根据标题查询下载历史记录
        :return:
        """
        return self.dbhelper.get_download_history_by_title(title=title) or []

    def get_download_history_by_downloader(self, downloader, download_id):
        """
        根据下载器和下载ID查询下载历史记录
        :return:
        """
        return self.dbhelper.get_download_history_by_downloader(downloader=downloader,
                                                                download_id=download_id)

    def update_downloader(self,
                          did,
                          name,
                          enabled,
                          dtype,
                          transfer,
                          only_nastool,
                          match_path,
                          rmt_mode,
                          config,
                          download_dir):
        """
        更新下载器
        """
        ret = self.dbhelper.update_downloader(
            did=did,
            name=name,
            enabled=enabled,
            dtype=dtype,
            transfer=transfer,
            only_nastool=only_nastool,
            match_path=match_path,
            rmt_mode=rmt_mode,
            config=config,
            download_dir=download_dir
        )
        self.init_config()
        return ret

    def delete_downloader(self, did):
        """
        删除下载器
        """
        ret = self.dbhelper.delete_downloader(did=did)
        self.init_config()
        return ret

    def check_downloader(self, did=None, transfer=None, only_nastool=None, enabled=None, match_path=None):
        """
        检查下载器
        """
        ret = self.dbhelper.check_downloader(did=did,
                                             transfer=transfer,
                                             only_nastool=only_nastool,
                                             enabled=enabled,
                                             match_path=match_path)
        self.init_config()
        return ret

    def delete_download_setting(self, sid):
        """
        删除下载设置
        """
        ret = self.dbhelper.delete_download_setting(sid=sid)
        self.init_config()
        return ret

    def update_download_setting(self,
                                sid,
                                name,
                                category,
                                tags,
                                is_paused,
                                upload_limit,
                                download_limit,
                                ratio_limit,
                                seeding_time_limit,
                                downloader):
        """
        更新下载设置
        """
        ret = self.dbhelper.update_download_setting(
            sid=sid,
            name=name,
            category=category,
            tags=tags,
            is_paused=is_paused,
            upload_limit=upload_limit,
            download_limit=download_limit,
            ratio_limit=ratio_limit,
            seeding_time_limit=seeding_time_limit,
            downloader=downloader
        )
        self.init_config()
        return ret

    def set_downloadspeed_limit(self, downloader_id, torrent_ids, limit):
        """
        设置种子的下载限速 kb/s
        """
        log.info(f"【Downloader】设置种子: %s 下载限速 %s" % (torrent_ids, limit))
        if not downloader_id:
            return
        _client = self.__get_client(downloader_id)
        if not _client:
            return

        _client.set_downloadspeed_limit(torrent_ids, limit)

    def fraction_download(self, fraction_rule, origin_limit, size, download_dir, downloader_id, download_id):
        """
        做种使用文件部分下载，开启最小化文件做种模式后使用
        fraction_rule: 部分下载规则
        size: rss 中种子总大小，后续会给出真实使用的大小
        :param download_dir: 下载目录
        :param origin_limit: 还有多少保种的体积没有使用
        return:
        result: 当前种子是否要触发下载
        real_size: 当前种子触发下载后，真实下载多大
        """
        gb2bytes = 1024*1024*1024
        log.info(f"【Downloader】部分下载开始, 剩余保种体积:%s GB" % str(round(origin_limit / gb2bytes, 2)))
        _client = self.__get_client(downloader_id)
        torrent_files = self.get_files(tid=download_id, downloader_id=downloader_id)
        downloader_conf = self.get_downloader_conf(downloader_id)
        if not torrent_files:
            return False, 0, "torrent content files get error"

        limit_size = size
        percent = 1
        # 根据规则挑选合适的文件
        try:
            if fraction_rule:
                before_range = fraction_rule.get("frac_before_range")
                before_percent = fraction_rule.get("frac_before_percent")
                after_range = fraction_rule.get("frac_after_range")
                if before_range and before_percent:
                    before_range_num = len(before_range.split(","))
                    before_percent_num = len(before_percent.split(","))
                    if before_range_num == before_percent_num:
                        target_range_index = -2
                        #
                        """
                        查找目标index，假设range为10,20,100，percent为 1, 0.5, 0.25
                        <10，index=-1, percent 为默认1，不裁切
                        10<size<20,index=0, percent=1
                        20<size<100,index=1, percent=0.5
                        100<size, index=-2, percent=0.25
                        """
                        before_range_array = before_range.split(",")
                        before_percent_array = before_percent.split(",")
                        for index in range(before_range_num):
                            if size <= int(before_range_array[index]) * gb2bytes:
                                target_range_index = index - 1
                                break
                        if target_range_index == -1:
                            # 命中最左边无界区间
                            percent = 1
                        elif target_range_index == -2:
                            # 命中最右边无界区间
                            percent = before_percent_array[before_range_num-1]
                        else:
                            percent = before_percent_array[target_range_index]

                        limit_size = size * float(percent)
                    else:
                        log.info(f"【部分下载】下载前设置区间个数不一致，range: {before_range} 个数{before_range_num} 和 percent: {before_percent} 个数{before_percent_num}，请修正！")

                # 计算后的尺寸限制
                if after_range:
                    if len(after_range.split(",")) == 2:
                        after_range_array = after_range.split(",")
                        limit_size = max(limit_size, int(after_range_array[0]) * gb2bytes)
                        limit_size = min(limit_size, int(after_range_array[1]) * gb2bytes)
                    else:
                        log.info(f"下载后设置格式错误，应该为 A,B 形式，请修正！")
        except Exception as e:
            ExceptionUtils.exception_traceback(e)
            log.error("【部分下载】部分下载规则参数出错：%s" % (str(e)))

        # 不要超过磁盘剩余空间，如果目录没找到，默认为0
        if not os.path.exists(download_dir):
            return False, 0, "下载目录不存在 %s，请检查磁盘空间或者是否挂载正常" %(str(download_dir))

        free_space = SystemUtils.get_free_space(download_dir)
        if free_space == 0:
            return False, 0, "%s 目录空间为 0，请检查磁盘空间或者是否挂载正常" % (str(download_dir))

        limit_size = min(origin_limit, limit_size)

        limit_size = min(limit_size, free_space * gb2bytes)

        # 计算出来的限制大小为0，跳过文件优先级处理
        if limit_size == 0:
            return False, 0, "torrent limit file size is 0"

        # for qb
        priority_info = {
            "high": [],
            "mid": [],
            "normal": [],
            "donotdownload": []
        }
        # for transmission
        transmission_priority_info = {}

        cur_size = 0
        for torrent_file in torrent_files:
            file_id = torrent_file.get("id")
            file_size = torrent_file.get("size")
            cur_size += file_size

            if cur_size > limit_size:
                priority_info['donotdownload'].append(file_id)
                transmission_priority_info[file_id] = {'priority': 'low', 'selected': False}
                # 加了当前文件超标了，不添加了，继续往下找合适的(多加的file_size记得减回去）
                cur_size -= file_size
                continue

            if cur_size < limit_size/3:
                priority_info['high'].append(file_id)
                transmission_priority_info[file_id] = {'priority': 'high', 'selected': True}
            elif cur_size < limit_size * 2 /3:
                priority_info['mid'].append(file_id)
                transmission_priority_info[file_id] = {'priority': 'normal', 'selected': True}
            elif cur_size <= limit_size:
                priority_info['normal'].append(file_id)
                transmission_priority_info[file_id] = {'priority': 'low', 'selected': True}

        log.info(f"【Downloader】部分下载添加文件: {priority_info}, 限制大小: {round(limit_size/gb2bytes, 2)}GB, 磁盘剩余空间: {round(free_space, 2)}GB")

        if downloader_conf.get("type") == "transmission":
            _client.set_files(file_info={download_id: transmission_priority_info})
        elif downloader_conf.get("type") == "qbittorrent":
            _client.set_files(torrent_hash=download_id, file_ids=priority_info['donotdownload'], priority=0)
            _client.set_files(torrent_hash=download_id, file_ids=priority_info['normal'], priority=1)
            _client.set_files(torrent_hash=download_id, file_ids=priority_info['mid'], priority=6)
            _client.set_files(torrent_hash=download_id, file_ids=priority_info['high'], priority=7)

        # 如果没有找到合适的文件，删除这个任务
        if cur_size == 0:
            return False, 0, "didn't find property files in torrent matched rule"

        return True, cur_size, "success"
