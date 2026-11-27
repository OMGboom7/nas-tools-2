from ast import literal_eval
import base64
import json
import os
import subprocess
import time
from threading import Lock

from apscheduler.schedulers.background import BackgroundScheduler
from apscheduler.triggers.cron import CronTrigger

import log
from app.downloader import Downloader
from app.helper import DbHelper, RssHelper
from app.media.meta import MetaInfo
from app.message import Message
from app.sites import Sites
from app.utils import StringUtils, ExceptionUtils
from app.utils.commons import singleton
from config import BRUSH_REMOVE_TORRENTS_INTERVAL, Config, run_go_helper


def _brush_download(operation, **values):
    return json.loads(run_go_helper("brush-download", operation, payload=json.dumps(values).encode(), timeout=30))


class _BrushSubmission:
    def __init__(self, task, title, enclosure, size, deadline, partial):
        self.request = dict(taskID=task["id"], siteID=int(task["site_id"]), downloader=str(task["downloader"]),
                            ruleSource=task["rss_rule_source"], feedURL=task["rss_url"], title=title,
                            enclosure=enclosure, size=size, deadline=deadline or "", partial=partial)
        self.token = None
        self.partial = partial
        self.fraction_rule = task.get("fraction_rule")
        self.fraction_source = task.get("fraction_rule_source")
        self.selected_size = 0
        self.pending = False

    def finish(self):
        if self.pending:
            raise subprocess.SubprocessError("刷流任务已启动，等待恢复记账")
        if self.partial:
            _brush_download("finish", token=self.token)

    def submit(self, content, config, directory, local_dir, tags, category, upload, download, ratio, minutes, media, save_path):
        request = dict(brush=self.request, rule=self.fraction_rule or {}, ruleSource=self.fraction_source,
                       clientType=config["type"], config=config["config"], directories=config["download_dir"],
                       directory=directory or "", localDir=local_dir or "", category=category or "", tags=tags,
                       uploadLimitKB=int(upload or 0), downloadLimitKB=int(download or 0),
                       ratioLimit=round(float(ratio or 0), 2), seedingTimeLimit=int(minutes or 0))
        if not self.partial:
            request.update(media=media, savePath=save_path)
        if isinstance(content, str):
            request["magnet"] = content
        else:
            request["torrent"] = base64.b64encode(content).decode("ascii")
        reply = json.loads(run_go_helper("brush-fraction" if self.partial else "brush-whole", payload=json.dumps(request).encode(), timeout=90))
        self.token = reply["Token"]
        self.selected_size = reply["Size"]
        self.pending = reply["Pending"]
        return reply["Task"]


@singleton
class BrushTask(object):
    message = None
    sites = None
    dbhelper = None
    rsshelper = None
    downloader = None
    _scheduler = None
    _brush_tasks = {}
    _torrents_cache = set()
    _cache_lock = Lock()
    _torrents_free_limit_cache = set()
    _qb_client = "qbittorrent"
    _tr_client = "transmission"

    def __init__(self):
        self.init_config()

    def init_config(self):
        self.dbhelper = DbHelper()
        self.rsshelper = RssHelper()
        self.message = Message()
        self.sites = Sites()
        self.downloader = Downloader()
        # 移除现有任务
        self.stop_service()
        # 读取刷流任务列表
        try:
            _brush_download("recover")
        except (OSError, ValueError, subprocess.SubprocessError):
            log.warn("【Brush】部分已确认下载的刷流记录仍待恢复")
        self.load_brushtasks()
        # 清理缓存
        self._torrents_cache = set()
        # 启动RSS任务
        if self._brush_tasks:
            self._scheduler = BackgroundScheduler(timezone=Config().get_timezone())
            for _, task in self._brush_tasks.items():
                # 任务状态：Y-正常，S-停止下载新种，N-完全停止
                if task.get("state") in ['Y', 'S'] \
                        and task.get("interval"):
                    cron = str(task.get("interval")).strip()
                    if cron.isdigit():
                        if task.get("state") == 'Y':
                            self._scheduler.add_job(func=self.check_task_rss,
                                                    args=[task.get("id")],
                                                    trigger='interval',
                                                    seconds=int(cron) * 60)
                    elif cron.count(" ") == 4:
                        if task.get("state") == 'Y':
                            try:
                                self._scheduler.add_job(func=self.check_task_rss,
                                                        args=[task.get("id")],
                                                        trigger=CronTrigger.from_crontab(cron))
                            except Exception as err:
                                log.error(f"任务 {task.get('name')} 运行周期格式不正确：{str(err)}")
                    else:
                        log.error(f"任务 {task.get('name')} 运行周期格式不正确")
            # 正常运行任务数
            running_task = len(self._scheduler.get_jobs())
            # 启动删种任务
            if running_task > 0:
                self._scheduler.add_job(func=self.remove_tasks_torrents,
                                        trigger='interval',
                                        seconds=BRUSH_REMOVE_TORRENTS_INTERVAL)
                # 启动
                self._scheduler.print_jobs()
                self._scheduler.start()

                log.info(f"{running_task} 个刷流服务正常启动")

    def load_brushtasks(self):
        """
        从数据库加载刷流任务
        """
        self._brush_tasks = {}
        brushtasks = self.dbhelper.get_brushtasks()
        if not brushtasks:
            return
        # 加载任务到内存
        for task in brushtasks:
            site_info = self.sites.get_sites(siteid=task.SITE)
            if site_info:
                site_url = StringUtils.get_base_url(site_info.get("signurl") or site_info.get("rssurl"))
            else:
                site_url = ""
            downloader_info = self.downloader.get_downloader_conf(task.DOWNLOADER)
            total_size = round(int(self.dbhelper.get_brushtask_totalsize(task.ID)) / (1024 ** 3), 1)
            self._brush_tasks[str(task.ID)] = {
                "id": task.ID,
                "name": task.NAME,
                "site": site_info.get("name"),
                "site_id": task.SITE,
                "interval": task.INTEVAL,
                "label": task.LABEL,
                "up_limit": task.UP_LIMIT,
                "dl_limit": task.DL_LIMIT,
                "savepath": task.SAVEPATH,
                "state": task.STATE,
                "downloader": task.DOWNLOADER,
                "downloader_name": downloader_info.get("name") if downloader_info else None,
                "transfer": True if task.TRANSFER == "Y" else False,
                "brushtask_free_limit_speed": True if task.BRUSHTASK_FREE_LIMIT_SPEED == "Y" else False,
                "brushtask_free_ddl_delete": True if task.BRUSHTASK_FREE_DDL_DELETE == "Y" else False,
                "sendmessage": True if task.SENDMESSAGE == "Y" else False,
                "free": task.FREELEECH,
                "rss_rule": literal_eval(task.RSS_RULE),
                "rss_rule_source": task.RSS_RULE or "",
                "remove_rule": literal_eval(task.REMOVE_RULE),
                "fraction_rule_source": task.FRACTION_RULE or "",
                "fraction_rule": literal_eval(task.FRACTION_RULE) if task.FRACTION_RULE else "",
                "seed_size": task.SEED_SIZE,
                "total_size": total_size,
                "rss_url": task.RSSURL if task.RSSURL else site_info.get("rssurl"),
                "rss_url_show": task.RSSURL,
                "cookie": site_info.get("cookie"),
                "ua": site_info.get("ua"),
                "download_count": task.DOWNLOAD_COUNT,
                "remove_count": task.REMOVE_COUNT,
                "download_size": StringUtils.str_filesize(task.DOWNLOAD_SIZE),
                "upload_size": StringUtils.str_filesize(task.UPLOAD_SIZE),
                "lst_mod_date": task.LST_MOD_DATE,
                "site_url": site_url
            }

    def get_brushtask_info(self, taskid=None):
        """
        读取刷流任务列表
        """
        self.load_brushtasks()
        if taskid:
            return self._brush_tasks.get(str(taskid)) or {}
        else:
            return self._brush_tasks if isinstance(self._brush_tasks, dict) else {}

    def check_task_rss(self, taskid):
        """
        检查RSS并添加下载，由定时服务调用
        :param taskid: 刷流任务的ID
        """
        if not taskid:
            return
        try:
            _brush_download("recover", taskID=int(taskid))
        except (OSError, ValueError, subprocess.SubprocessError):
            log.warn("【Brush】部分已确认下载的刷流记录仍待恢复")
            return
        # 任务信息
        taskinfo = self.get_brushtask_info(taskid)
        if not taskinfo:
            return
        # 任务属性
        task_name = taskinfo.get("name")
        site_id = taskinfo.get("site_id")
        rss_url = taskinfo.get("rss_url")
        rss_rule = taskinfo.get("rss_rule")
        fraction_rule = taskinfo.get("fraction_rule")
        cookie = taskinfo.get("cookie")
        rss_free = taskinfo.get("free")
        state = taskinfo.get("state")
        if state != 'Y':
            log.info("【Brush】刷流任务 %s 已停止下载新种！" % task_name)
            return
        # 查询站点信息
        site_info = self.sites.get_sites(siteid=site_id)
        if not site_info:
            log.error("【Brush】刷流任务 %s 的站点已不存在，无法刷流！" % task_name)
            return
        # 站点属性
        site_id = site_info.get("id")
        site_name = site_info.get("name")
        site_proxy = site_info.get("proxy")
        site_brush_enable = site_info.get("brush_enable")
        if not site_brush_enable:
            log.error("【Brush】站点 %s 未开启刷流功能，无法刷流！" % site_name)
            return
        if not rss_url:
            log.error("【Brush】站点 %s 未配置RSS订阅地址，无法刷流！" % site_name)
            return
        if rss_free and not cookie:
            log.warn("【Brush】站点 %s 未配置Cookie，无法开启促销刷流" % site_name)
            return
        log.info("【Brush】开始站点 %s 的刷流任务：%s..." % (site_name, task_name))

        def available_slots():
            payload = {"brush": {"taskID": int(taskid)}}
            try:
                return json.loads(run_go_helper("brush-admission", payload=json.dumps(payload).encode(), timeout=90))
            except (OSError, ValueError, subprocess.SubprocessError):
                log.warn(f"【Brush】{task_name} 无法核对新增任务限制，下轮重试")
                return False

        scan_limit = available_slots()
        if not scan_limit:
            return

        rss_result = self.rsshelper.parse_rssxml(url=rss_url, proxy=site_proxy)
        if rss_result is None:
            # RSS链接过期
            log.error(f"【Brush】{task_name} RSS链接已过期，请重新获取！")
            return
        if len(rss_result) == 0:
            log.warn("【Brush】%s RSS未下载到数据" % site_name)
            return
        else:
            log.info("【Brush】%s RSS获取数据：%s" % (site_name, len(rss_result)))

        success_count = 0
        for res in rss_result:
            try:
                # 种子名
                torrent_name = res.get('title')
                # 种子链接
                enclosure = res.get('enclosure')
                # 种子页面
                page_url = res.get('link')
                # 种子大小
                size = res.get('size')
                # 发布时间
                pubdate = res.get('pubdate')

                cache_key = (taskid, enclosure)
                with self._cache_lock:
                    cache = self._torrents_cache
                    if cache_key in cache:
                        log.debug("【Brush】%s 已处理过" % torrent_name)
                        continue
                    cache.add(cache_key)

                # 检查种子种包含的免费限时信息
                try:
                    catalog = os.environ.get("NASTOOL_SITE_CATALOG") or Config().get_user_sites_bin_path()
                    detail = {"taskID": int(taskid), "siteID": int(site_id), "feedURL": rss_url,
                              "title": res.get("detail_title") or torrent_name, "pageURL": page_url or "",
                              "enclosure": enclosure, "selection": {"rules": rss_rule, "title": torrent_name,
                              "size": size, "published": pubdate.astimezone().isoformat() if pubdate else None}}
                    torrent_attr = json.loads(run_go_helper("brush-detail", catalog,
                                                            payload=json.dumps(detail).encode(), timeout=90))
                except (OSError, ValueError, TypeError, subprocess.SubprocessError):
                    with self._cache_lock:
                        cache.discard(cache_key)
                    log.warn("【Brush】%s 暂时无法核实种子详情，下次扫描重试" % task_name)
                    continue

                # 检查种子是否符合选种规则
                decision = torrent_attr["decision"]
                if decision == "retry":
                    with self._cache_lock:
                        cache.discard(cache_key)
                if decision != "match":
                    continue
                # 开始下载
                log.debug("【Brush】%s 符合条件，开始下载..." % torrent_name)
                if self.__download_torrent(taskinfo=taskinfo,
                                           rss_rule=rss_rule,
                                           fraction_rule=fraction_rule,
                                           site_info=site_info,
                                           title=torrent_name,
                                           enclosure=enclosure,
                                           size=size,
                                           torrent_attr=torrent_attr,
                                           ):
                    # 计数
                    success_count += 1
                    if (scan_limit > 0 and success_count >= scan_limit) or not available_slots():
                        break
                else:
                    with self._cache_lock:
                        cache.discard(cache_key)
            except (OSError, subprocess.SubprocessError):
                log.warn("【Brush】本轮刷流暂停，下次扫描先核对已确认的下载")
                return
            except Exception as err:
                ExceptionUtils.exception_traceback(err)
                continue
        log.info("【Brush】任务 %s 本次添加了 %s 个下载" % (task_name, success_count))

    def remove_tasks_torrents(self):
        """
        根据条件检查所有任务下载完成的种子，按条件进行删除，并更新任务数据
        由定时服务调用
        """

        def __send_message(_task_name, _delete_type, _torrent_name, _download_name, _torrent_size,
                           _download_size, _upload_size, _ratio, _add_time):
            """
            发送删种消息
            """
            _msg_title = f"【刷流任务 {_task_name} 删除做种】"
            _msg_text = f"下载器名：{_download_name}\n" \
                        f"种子名称：{_torrent_name}\n" \
                        f"种子大小：{_torrent_size}\n" \
                        f"已下载量：{_download_size}\n" \
                        f"已上传量：{_upload_size}\n" \
                        f"分享比率：{_ratio}\n" \
                        f"添加时间：{_add_time}\n" \
                        f"删除时间：{time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(time.time()))}\n" \
                        f"删除规则：{_delete_type}"
            self.message.send_brushtask_remove_message(title=_msg_title, text=_msg_text)

        log.debug("【Brush】删种任务检查开始运行")
        # 遍历所有任务
        for taskid, taskinfo in self._brush_tasks.items():
            if taskinfo.get("state") == 'N':
                continue
            try:
                # 总上传量
                total_uploaded = 0
                # 总下载量
                total_downloaded = 0
                # 可以删种的种子
                delete_ids = []
                # 需要更新状态的种子
                update_torrents = []
                # 任务信息
                task_name = taskinfo.get("name")
                downloader_id = taskinfo.get("downloader")
                sendmessage = taskinfo.get("sendmessage")

                # 当前任务种子详情
                task_torrents = self.get_brushtask_torrents(taskid)
                torrent_ids = [item.DOWNLOAD_ID for item in task_torrents if item.DOWNLOAD_ID and str(item.DOWNLOADER) == str(downloader_id)]
                # 避免种子被全删，没有种子ID的不处理
                if not torrent_ids:
                    continue
                # 下载器参数
                downloader_cfg = self.downloader.get_downloader_conf(downloader_id)
                if not downloader_cfg:
                    log.warn("【Brush】任务 %s 下载器不存在" % task_name)
                    continue
                # 下载器的类型
                downloader_type = downloader_cfg.get("type")
                if downloader_type not in ("qbittorrent", "transmission"):
                    continue
                completed = self.downloader.get_completed_torrents(downloader_id=downloader_id, ids=torrent_ids)
                downloading = self.downloader.get_downloading_torrents(downloader_id=downloader_id, ids=torrent_ids)
                if completed is None or downloading is None:
                    continue

                observations = []
                by_id = {}
                for complete, group in ((True, completed), (False, downloading)):
                    for torrent in group:
                        info = self.__get_torrent_dict(downloader_type, torrent)
                        tid = info["id"]
                        name = torrent.get("name") if downloader_type == "qbittorrent" else torrent.name
                        by_id[tid] = (name, info)
                        observations.append(dict(id=tid, completed=complete, seedSeconds=info["seeding_time"],
                                                 ageSeconds=info["dltime"], inactiveSeconds=info["iatime"],
                                                 averageUpload=info["avg_upspeed"], uploaded=info["uploaded"],
                                                 size=info["total_size"], ratio=round(info["ratio"], 2)))
                request = dict(taskID=int(taskid), downloader=str(downloader_id), items=observations)
                decisions = json.loads(run_go_helper("brush-maintenance", payload=json.dumps(request).encode(),
                                                     timeout=30))
                for tid, (name, info) in by_id.items():
                    decision = decisions[tid]
                    uploaded, downloaded = info["uploaded"], info["downloaded"]
                    total_uploaded += uploaded
                    total_downloaded += downloaded
                    if decision["Action"] == "remove":
                        delete_ids.append(tid)
                        update_torrents.append((f"{uploaded},{downloaded}", taskid, tid))
                        if sendmessage:
                            __send_message(task_name, decision["Reason"], name, downloader_cfg.get("name"),
                                           StringUtils.str_filesize(info["total_size"]), StringUtils.str_filesize(downloaded),
                                           StringUtils.str_filesize(uploaded), round(info["ratio"], 2), info["add_time"])
                    elif decision["Action"] == "throttle":
                        key = (downloader_id, taskid, tid, decision["Deadline"])
                        if key not in self._torrents_free_limit_cache:
                            self.downloader.set_downloadspeed_limit(downloader_id, tid, 1)
                            self._torrents_free_limit_cache.add(key)
                            if sendmessage:
                                self.message.send_brushtask_remove_message(
                                    f"【刷流任务 {task_name} 限免结束】",
                                    f"开启下载限速，限免截止时间：{decision['Deadline']}，种子名称：{name}")

                # 暂停或校验中的任务可能不在上述两个列表。查询全部状态后再清理历史。
                remove_torrent_ids = set(torrent_ids) - by_id.keys()
                if remove_torrent_ids:
                    remaining = self.downloader.get_torrents(downloader_id=downloader_id, ids=list(remove_torrent_ids))
                    if remaining is None:
                        remove_torrent_ids.clear()
                    else:
                        remove_torrent_ids.difference_update(t.get("hash") if downloader_type == "qbittorrent"
                                                             else t.hashString for t in remaining)
                    for tid in remove_torrent_ids:
                        self.dbhelper.delete_brushtask_torrent(taskid, tid, downloader_id)

                # 删除下载器种子
                if delete_ids:
                    self.downloader.delete_torrents(downloader_id=downloader_id,
                                                    ids=delete_ids,
                                                    delete_file=True)
                    # 检验下载器中种子是否已经删除
                    time.sleep(5)
                    torrents = self.downloader.get_torrents(downloader_id=downloader_id, ids=delete_ids)
                    if torrents is None:
                        log.debug("【Brush】任务 %s 从下载器获取种子为 None" % task_name)
                        delete_ids = []
                        update_torrents = []
                    else:
                        for torrent in torrents:
                            torrent_info = self.__get_torrent_dict(downloader_type=downloader_type,
                                                                   torrent=torrent)
                            # ID
                            torrent_id = torrent_info.get("id")
                            # 依然存在下载器的种子移出删除列表
                            if torrent_id in delete_ids:
                                delete_ids.remove(torrent_id)
                    if delete_ids:
                        # 更新种子状态为已删除
                        update_torrents = [update_torrent for update_torrent in update_torrents
                                           if update_torrent[2] in delete_ids]
                        self.dbhelper.update_brushtask_torrent_state(update_torrents, downloader_id)
                        log.info("【Brush】任务 %s 共删除 %s 个刷流下载任务" % (task_name, len(delete_ids)))
                    else:
                        log.info("【Brush】任务 %s 本次检查未删除下载任务" % task_name)
                # 更新上传下载量和删除种子数
                self.dbhelper.add_brushtask_upload_count(brush_id=taskid,
                                                         upload_size=total_uploaded,
                                                         download_size=total_downloaded,
                                                         remove_count=len(delete_ids) + len(remove_torrent_ids))
            except Exception as e:
                ExceptionUtils.exception_traceback(e)

    def __download_torrent(self,
                           taskinfo,
                           rss_rule,
                           fraction_rule,
                           site_info,
                           title,
                           enclosure,
                           size,
                           torrent_attr,
                           ):
        """
        添加下载任务，更新任务数据
        :param taskinfo: 任务信息
        :param rss_rule: rss规则
        :param fraction_rule: 部分下载规则
        :param site_info: 站点信息
        :param title: 种子名称
        :param enclosure: 种子地址
        :param size: 种子大小
        :param torrent_attr: 种子属性
        """
        if not enclosure:
            return False
        # 站点流控
        if self.sites.check_ratelimit(site_info.get("id")):
            return False
        taskname = taskinfo.get("name")
        transfer = taskinfo.get("transfer")
        sendmessage = taskinfo.get("sendmessage")
        downloader_id = taskinfo.get("downloader")
        download_limit = rss_rule.get("downspeed")
        upload_limit = rss_rule.get("upspeed")
        download_dir = taskinfo.get("savepath")
        tag = taskinfo.get("label").split(',') if taskinfo.get("label") else None
        # 标签
        if not transfer:
            if tag:
                tag += ["已整理"]
            else:
                tag = ["已整理"]

        # 开始下载
        meta_info = MetaInfo(title=title)
        meta_info.set_torrent_info(site=site_info.get("name"),
                                   enclosure=enclosure,
                                   size=size)

        fraction_rule_exits = self.__is_fraction_rule_set(fraction_rule)

        submission = _BrushSubmission(taskinfo, title, enclosure, size, torrent_attr.get("free_deadline"),
                                      fraction_rule_exits)
        _, download_id, dir, retmsg = self.downloader.download(
            media_info=meta_info,
            brush_submission=submission,
            tag=tag,
            is_paused=(True if fraction_rule_exits else False),
            downloader_id=downloader_id,
            download_dir=download_dir,
            download_setting="-2",
            download_limit=download_limit,
            upload_limit=upload_limit,
            skip_size_check=(True if fraction_rule_exits else False)
        )

        if not download_id:
            # 下载失败
            log.warn(f"【Brush】{taskname} 添加下载任务出错：{title}，"
                     f"错误原因：{retmsg or '下载器添加任务失败'}，"
                     f"种子链接：{enclosure}")
            return False
        else:
            # 下载种子文件成功，开始部分下载功能
            log.info("【Brush】成功添加下载：id: %s, title: %s" % (download_id, title))
            real_size = submission.selected_size if fraction_rule_exits else size

            try:
                submission.finish()
            except (OSError, ValueError, subprocess.SubprocessError):
                log.warn("【Brush】下载器已接收，刷流历史和次数将在下轮扫描恢复")
                raise

            if sendmessage:
                # 下载器参数
                downloader_cfg = self.downloader.get_downloader_conf(downloader_id)
                # 下载器名称
                downlaod_name = downloader_cfg.get("name")
                msg_title = f"【刷流任务 {taskname} 新增下载】"
                msg_text = f"下载器名：{downlaod_name}\n" \
                           f"种子名称：{title}\n" \
                           f"种子大小：{StringUtils.str_filesize(real_size)}\n" \
                           f"添加时间：{time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(time.time()))}"
                self.message.send_brushtask_added_message(title=msg_title, text=msg_text)

        return True

    def __is_fraction_rule_set(self, fraction_rule):
        if fraction_rule:
            before_range = fraction_rule.get("frac_before_range")
            before_percent = fraction_rule.get("frac_before_percent")
            after_range = fraction_rule.get("frac_after_range")

            if before_range or before_percent or after_range:
                return True

        return False

    @staticmethod
    def __get_torrent_dict(downloader_type, torrent):

        # 当前时间戳
        date_now = int(time.time())

        if downloader_type == "qbittorrent":
            # ID
            torrent_id = torrent.get("hash")
            # 下载时间
            dltime = date_now - torrent.get("added_on") if (torrent.get("added_on") or 0) > 0 else 0
            # 做种时间
            seeding_time = date_now - torrent.get("completion_on") if (torrent.get("completion_on") or 0) > 0 else 0
            # 分享率
            ratio = torrent.get("ratio") or 0
            # 上传量
            uploaded = torrent.get("uploaded") or 0
            # 平均上传速度 Byte/s
            if dltime:
                avg_upspeed = int(uploaded / dltime)
            else:
                avg_upspeed = uploaded
            # 已未活动 秒
            iatime = date_now - torrent.get("last_activity") if (torrent.get("last_activity") or 0) > 0 else 0
            # 下载量
            downloaded = torrent.get("downloaded")
            # 种子大小
            total_size = torrent.get("total_size")
            # 添加时间
            add_time = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(torrent.get("added_on") or 0))

            # tag
            tags = torrent.get("tags")
        else:

            # ID
            torrent_id = torrent.hashString
            # 做种时间
            if not hasattr(torrent, 'date_done') or not torrent.date_done or torrent.date_done.timestamp() < 1:
                seeding_time = 0
            else:
                seeding_time = date_now - int(torrent.date_done.timestamp())
            # 下载耗时
            if not torrent.date_added or torrent.date_added.timestamp() < 1:
                dltime = 0
            else:
                dltime = date_now - int(torrent.date_added.timestamp())
            # 下载量
            downloaded = int(torrent.total_size * torrent.progress / 100)
            # 分享率
            ratio = torrent.ratio or 0
            # 上传量
            uploaded = int(downloaded * torrent.ratio)
            # 平均上传速度
            if dltime:
                avg_upspeed = int(uploaded / dltime)
            else:
                avg_upspeed = uploaded
            # 未活动时间
            if not torrent.date_active or torrent.date_active.timestamp() < 1:
                iatime = 0
            else:
                iatime = date_now - int(torrent.date_active.timestamp())
            # 种子大小
            total_size = torrent.total_size
            # 添加时间
            add_time = time.strftime('%Y-%m-%d %H:%M:%S',
                                     time.localtime(torrent.date_added.timestamp() if torrent.date_added else 0))
            # tags
            tags = torrent.labels

        return {
            "id": torrent_id,
            "seeding_time": seeding_time,
            "ratio": ratio,
            "uploaded": uploaded,
            "downloaded": downloaded,
            "avg_upspeed": avg_upspeed,
            "iatime": iatime,
            "dltime": dltime,
            "total_size": total_size,
            "add_time": add_time,
            "tags": tags if tags else ""
        }

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

    def update_brushtask(self, brushtask_id, item):
        """
        新增刷种任务
        """
        ret = self.dbhelper.update_brushtask(brushtask_id, item)
        self.init_config()
        return ret

    def delete_brushtask(self, brushtask_id):
        """
        删除刷种任务
        """
        ret = self.dbhelper.delete_brushtask(brushtask_id)
        self.init_config()
        return ret

    def update_brushtask_state(self, state, brushtask_id=None):
        """
        更新刷种任务状态
        """
        ret = self.dbhelper.update_brushtask_state(tid=brushtask_id, state=state)
        self.init_config()
        return ret

    def get_brushtask_torrents(self, brush_id, active=True):
        """
        获取刷种任务的种子列表
        """
        return self.dbhelper.get_brushtask_torrents(brush_id, active)
