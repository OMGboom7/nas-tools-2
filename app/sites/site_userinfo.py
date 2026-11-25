import json
import subprocess
from datetime import datetime
from threading import Lock

import log
from app.helper import DbHelper
from app.message import Message
from app.sites.sites import Sites
from app.utils import StringUtils
from app.utils.commons import singleton
from config import Config, run_go_helper

lock = Lock()


def read_site_statistics(action, data=None):
    params = dict(data or {})
    if isinstance(params.get("sites"), str):
        params["sites"] = [params["sites"]] if params["sites"] else []
    try:
        raw = run_go_helper("site-statistics-read", Config().get_user_sites_bin_path(), action,
                            payload=json.dumps(params).encode())
        return json.loads(raw)
    except (OSError, ValueError, subprocess.SubprocessError):
        return {"code": 1, "msg": "站点统计查询不可用"}


@singleton
class SiteUserInfo(object):
    sites = None
    dbhelper = None
    message = None

    _last_update_time = None
    _sites_data = {}

    def __init__(self):
        self.init_config()

    def init_config(self):
        self.sites = Sites()
        self.dbhelper = DbHelper()
        self.message = Message()
        # 站点上一次更新时间
        self._last_update_time = None
        # 站点数据
        self._sites_data = {}

    def __notify_unread_msg(self, site_name, count, messages):
        if not count or self._sites_data.get(site_name, {}).get("message_unread") == count:
            return
        if messages:
            for item in messages:
                msg_title = f"【站点 {site_name} 消息】"
                msg_text = f"时间：{item['date']}\n标题：{item['title']}\n内容：\n{item['content']}"
                self.message.send_site_message(title=msg_title, text=msg_text)
        else:
            self.message.send_site_message(
                title=f"站点 {site_name} 收到 {count} 条新消息，请登陆查看")

    def refresh_site_data_now(self):
        """
        强制刷新站点数据
        """
        self.__refresh_all_site_data(force=True)
        # 刷完发送消息
        string_list = []

        # 增量数据
        incUploads = 0
        incDownloads = 0
        result = read_site_statistics("get_site_history", {"days": 1})
        if result["code"]:
            log.error("【Sites】" + result["msg"])
            return
        for site, upload, download in result["dataset"][1:]:
            if upload > 0 or download > 0:
                incUploads += int(upload)
                incDownloads += int(download)
                string_list.append(f"【{site}】\n"
                                   f"上传量：{StringUtils.str_filesize(upload)}\n"
                                   f"下载量：{StringUtils.str_filesize(download)}\n"
                                   f"\n————————————")

        if incDownloads or incUploads:
            string_list.insert(0, f"【今日汇总】\n"
                                  f"总上传：{StringUtils.str_filesize(incUploads)}\n"
                                  f"总下载：{StringUtils.str_filesize(incDownloads)}\n"
                                  f"\n————————————")

            self.message.send_user_statistics_message(string_list)

    def get_site_data(self, specify_sites=None, force=False):
        """
        获取站点上传下载量
        """
        self.__refresh_all_site_data(force=force, specify_sites=specify_sites)
        return self._sites_data

    def __refresh_all_site_data(self, force=False, specify_sites=None):
        """
        多线程刷新站点下载上传量，默认间隔6小时
        """
        if not self.sites.get_sites():
            return

        log.info("【Sites】Attempting to acquire lock for refresh...")
        with lock:
            log.info("【Sites】Lock acquired for refresh.")

            start_time = datetime.now()
            log.info(f"【Sites】开始刷新所有站点数据... Force: {force}, Sites: {specify_sites or 'All'}")

            if not force \
                    and not specify_sites \
                    and self._last_update_time:
                return

            if specify_sites \
                    and not isinstance(specify_sites, list):
                specify_sites = [specify_sites]

            # 没有指定站点，默认使用全部站点
            if not specify_sites:
                refresh_sites = self.sites.get_sites(statistic=True)
            else:
                refresh_sites = [site for site in self.sites.get_sites(statistic=True) if
                                 site.get("name") in specify_sites]

            if not refresh_sites:
                log.info("【Sites】没有需要刷新的站点，任务退出。")
                return

            targets = [{"id": site["id"], "name": site["name"], "url": site["strict_url"]}
                       for site in refresh_sites if not self.sites.check_ratelimit(site["id"])]
            if not targets:
                return
            try:
                rows = json.loads(run_go_helper("site-statistics", payload=json.dumps(targets).encode(), timeout=None))
            except (OSError, ValueError, subprocess.SubprocessError):
                log.error("【Sites】统计任务未能完成，请查看原站点配置")
                return
            for row in rows:
                name, data = row["name"], row["data"]
                if row.get("notify"):
                    try:
                        self.__notify_unread_msg(name, data["message_unread"], row.get("messages"))
                    except Exception:
                        log.error(f"【Sites】{name} 未读消息通知失败")
                        continue
                self._sites_data[name] = data
            self.sites.init_favicons()

            # 更新时间
            self._last_update_time = datetime.now()
            log.info(f"【Sites】所有站点数据刷新流程结束，总耗时: {round((datetime.now() - start_time).total_seconds(), 2)}s")

    def update_site_name(self, old_name, name):
        """
        更新站点数据中的站点名称
        """
        self.dbhelper.update_site_user_statistics_site_name(name, old_name)
        self.dbhelper.update_site_seed_info_site_name(name, old_name)
        self.dbhelper.update_site_statistics_site_name(name, old_name)
        return True

    def is_min_join_date_beyond_one_month(self):
        """
        查询最早加入PT站的时间是否超过一个月
        """
        # 读取强制刷流配置
        _force_enable_brush = Config().get_config("pt").get("force_enable_brush")
        if _force_enable_brush:
            return True

        first_pt_site = read_site_statistics("min_join_date").get("data")
        if not first_pt_site or not StringUtils.is_one_month_ago(first_pt_site):
            return False
        else:
            return True