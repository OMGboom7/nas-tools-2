import datetime

from apscheduler.executors.pool import ThreadPoolExecutor
from apscheduler.schedulers.background import BackgroundScheduler

import log
from app.mediaserver import MediaServer
from app.rss import Rss
from app.sites.site_userinfo import StatisticsScheduler
from app.subscribe import Subscribe
from app.sync import Sync
from app.utils import ExceptionUtils
from app.utils.commons import singleton
from config import SYNC_TRANSFER_INTERVAL, RSS_CHECK_INTERVAL, \
    RSS_REFRESH_TMDB_INTERVAL, REFRESH_WALLPAPER_INTERVAL, Config, run_go_helper
from web.backend.wallpaper import get_login_wallpaper


@singleton
class Scheduler:
    SCHEDULER = None
    _statistics_timer = None
    _pt = None
    _douban = None
    _media = None

    def __init__(self):
        self.init_config()

    def init_config(self):
        self._pt = Config().get_config('pt')
        self._media = Config().get_config('media')
        self._stop_jobs()
        self.run_service()

    def run_service(self):
        """
        读取配置，启动定时服务
        """
        self.SCHEDULER = BackgroundScheduler(timezone=Config().get_timezone(),
                                             executors={
                                                 'default': ThreadPoolExecutor(20)
                                             })
        if not self.SCHEDULER:
            return
        self.SCHEDULER.add_job(run_go_helper, "interval", seconds=60, args=["classic-download", "notices"],
                               kwargs={"timeout": 60}, next_run_time=datetime.datetime.now(), max_instances=1)
        if self._pt:
            # RSS下载器
            pt_check_interval = self._pt.get('pt_check_interval')
            if pt_check_interval:
                if isinstance(pt_check_interval, str) and pt_check_interval.isdigit():
                    pt_check_interval = int(pt_check_interval)
                else:
                    try:
                        pt_check_interval = round(float(pt_check_interval))
                    except Exception as e:
                        log.error("RSS订阅周期 配置格式错误：%s" % str(e))
                        pt_check_interval = 0
                if pt_check_interval:
                    if pt_check_interval < 300:
                        pt_check_interval = 300
                    self.SCHEDULER.add_job(Rss().rssdownload, 'interval', seconds=pt_check_interval)
                    log.info("RSS订阅服务启动")

            # RSS订阅定时搜索
            search_rss_interval = self._pt.get('search_rss_interval')
            if search_rss_interval:
                if isinstance(search_rss_interval, str) and search_rss_interval.isdigit():
                    search_rss_interval = int(search_rss_interval)
                else:
                    try:
                        search_rss_interval = round(float(search_rss_interval))
                    except Exception as e:
                        log.error("订阅定时搜索周期 配置格式错误：%s" % str(e))
                        search_rss_interval = 0
                if search_rss_interval:
                    if search_rss_interval < 6:
                        search_rss_interval = 6
                    self.SCHEDULER.add_job(Subscribe().subscribe_search_all, 'interval', hours=search_rss_interval)
                    log.info("订阅定时搜索服务启动")

        # 媒体库同步
        if self._media:
            mediasync_interval = self._media.get("mediasync_interval")
            if mediasync_interval:
                if isinstance(mediasync_interval, str):
                    if mediasync_interval.isdigit():
                        mediasync_interval = int(mediasync_interval)
                    else:
                        try:
                            mediasync_interval = round(float(mediasync_interval))
                        except Exception as e:
                            log.info("豆瓣同步服务启动失败：%s" % str(e))
                            mediasync_interval = 0
                if mediasync_interval and self._is_scheduler_valid():
                    self.SCHEDULER.add_job(MediaServer().sync_mediaserver, 'interval', hours=mediasync_interval)
                    log.info("媒体库同步服务启动")
                else:
                    log.error("【SCHEDULE】sync_mediaserver 任务调度器未成功初始化或已关闭，无法添加新任务")

        # 定时把队列中的监控文件转移走
        if self._is_scheduler_valid():
            self.SCHEDULER.add_job(Sync().transfer_mon_files, 'interval', seconds=SYNC_TRANSFER_INTERVAL)
        else:
            log.error("【SCHEDULE】transfer_mon_files 任务调度器未成功初始化或已关闭，无法添加新任务")

        # RSS队列中搜索
        if self._is_scheduler_valid():
            self.SCHEDULER.add_job(Subscribe().subscribe_search, 'interval', seconds=RSS_CHECK_INTERVAL)
        else:
            log.error("【SCHEDULE】subscribe_search 任务调度器未成功初始化或已关闭，无法添加新任务")

        # 豆瓣RSS转TMDB，定时更新TMDB数据
        if self._is_scheduler_valid():
            self.SCHEDULER.add_job(Subscribe().refresh_rss_metainfo, 'interval', hours=RSS_REFRESH_TMDB_INTERVAL)
        else:
            log.error("【SCHEDULE】refresh_rss_metainfo 任务调度器未成功初始化或已关闭，无法添加新任务")

        # 定时刷新壁纸
        if self._is_scheduler_valid():
            self.SCHEDULER.add_job(get_login_wallpaper, 'interval', hours=REFRESH_WALLPAPER_INTERVAL, next_run_time=datetime.datetime.now())
        else:
            log.error("【SCHEDULE】get_login_wallpaper 任务调度器未成功初始化或已关闭，无法添加新任务")

        if self._is_scheduler_valid():
            self.SCHEDULER.print_jobs()
            self.SCHEDULER.start()
            if self._statistics_timer is None:
                self._statistics_timer = StatisticsScheduler()
                self._statistics_timer.start()
        else:
            log.error("【SCHEDULE】任务调度器未成功初始化或已关闭，无法执行打印job任务和开始任务")

    def _is_scheduler_valid(self):
        return self.SCHEDULER and not self.SCHEDULER.state == 'shutdown'

    def stop_service(self):
        """
        停止定时服务
        """
        if self._statistics_timer is not None:
            self._statistics_timer.stop()
            self._statistics_timer = None
        self._stop_jobs()

    def _stop_jobs(self):
        try:
            if self.SCHEDULER:
                self.SCHEDULER.remove_all_jobs()
                self.SCHEDULER.shutdown()
                self.SCHEDULER = None
        except Exception as e:
            ExceptionUtils.exception_traceback(e)
