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
        _, _, site, upload, download = SiteUserInfo().get_pt_site_statistics_history(2)

        # 按照上传降序排序
        data_list = list(zip(site, upload, download))
        data_list = sorted(data_list, key=lambda x: x[1], reverse=True)

        for data in data_list:
            site = data[0]
            upload = int(data[1])
            download = int(data[2])
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

    def get_pt_site_statistics_history(self, days=7, end_day=None):
        """
        获取站点上传下载量
        """
        site_urls = []
        for site in self.sites.get_sites(statistic=True):
            site_url = site.get("strict_url")
            if site_url:
                site_urls.append(site_url)

        return self.dbhelper.get_site_statistics_recent_sites(days=days, end_day=end_day, strict_urls=site_urls)

    def get_site_user_statistics(self, sites=None, encoding="RAW"):
        """
        获取站点用户数据
        :param sites: 站点名称
        :param encoding: RAW/DICT
        :return:
        """
        statistic_sites = self.sites.get_sites(statistic=True)
        if not sites:
            site_urls = [site.get("strict_url") for site in statistic_sites]
        else:
            site_urls = [site.get("strict_url") for site in statistic_sites
                         if site.get("name") in sites]

        raw_statistics = self.dbhelper.get_site_user_statistics(strict_urls=site_urls)
        if encoding == "RAW":
            return raw_statistics

        return self.__todict(raw_statistics)

    def get_pt_site_activity_history(self, site, days=365 * 2):
        """
        查询站点 上传，下载，做种数据
        :param site: 站点名称
        :param days: 最大数据量
        :return:
        """
        site_activities = [["time", "upload", "download", "bonus", "seeding", "seeding_size"]]
        sql_site_activities = self.dbhelper.get_site_statistics_history(site=site, days=days)
        for sql_site_activity in sql_site_activities:
            timestamp = datetime.strptime(sql_site_activity.DATE, '%Y-%m-%d').timestamp() * 1000
            site_activities.append(
                [timestamp,
                 sql_site_activity.UPLOAD,
                 sql_site_activity.DOWNLOAD,
                 sql_site_activity.BONUS,
                 sql_site_activity.SEEDING,
                 sql_site_activity.SEEDING_SIZE])

        return site_activities

    def get_pt_site_seeding_info(self, site):
        """
        查询站点 做种分布信息
        :param site: 站点名称
        :return: seeding_info:[uploader_num, seeding_size]
        """
        site_seeding_info = {"seeding_info": []}
        seeding_info = self.dbhelper.get_site_seeding_info(site=site)
        if not seeding_info:
            return site_seeding_info

        site_seeding_info["seeding_info"] = json.loads(seeding_info[0])
        return site_seeding_info

    def get_pt_site_min_join_date(self, sites=None):
        """
        查询站点加入时间
        """
        statistics = self.get_site_user_statistics(sites=sites, encoding="DICT")
        if not statistics:
            return ""
        dates = []
        for s in statistics:
            if s.get("join_at"):
                try:
                    dates.append(datetime.strptime(s.get("join_at"), '%Y-%m-%d %H:%M:%S'))
                except Exception as err:
                    print(str(err))
                    pass
        if dates:
            return min(dates).strftime("%Y-%m-%d")
        return ""

    @staticmethod
    def __todict(raw_statistics):
        statistics = []
        for site in raw_statistics:
            statistics.append({"site": site.SITE,
                               "username": site.USERNAME,
                               "user_level": site.USER_LEVEL,
                               "join_at": site.JOIN_AT,
                               "last_seen": site.LAST_SEEN,
                               "update_at": site.UPDATE_AT,
                               "upload": site.UPLOAD,
                               "download": site.DOWNLOAD,
                               "ratio": site.RATIO,
                               "seeding": site.SEEDING,
                               "leeching": site.LEECHING,
                               "seeding_size": site.SEEDING_SIZE,
                               "bonus": site.BONUS,
                               "url": site.URL,
                               "msg_unread": site.MSG_UNREAD
                               })
        return statistics

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

        first_pt_site = self.get_pt_site_min_join_date()
        if not first_pt_site or not StringUtils.is_one_month_ago(first_pt_site):
            return False
        else:
            return True