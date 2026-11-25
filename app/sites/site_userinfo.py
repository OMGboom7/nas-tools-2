import json
import subprocess
from threading import Event, Lock, Thread

import log
from app.helper import DbHelper
from app.message import MessageCenter
from jinja2.sandbox import SandboxedEnvironment
from app.sites.sites import Sites
from app.utils import StringUtils
from app.utils.commons import singleton
from config import Config, go_helper_path, run_go_helper

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


def _serve_refresh(worker, selected=None):
    sites = Sites()
    targets = [{"id": site["id"], "name": site["name"], "url": site["strict_url"]}
               for site in sites.get_sites(statistic=True)
               if (not selected or site["name"] in selected) and not sites.check_ratelimit(site["id"])]
    worker.stdin.write(json.dumps(targets) + "\n")
    worker.stdin.flush()
    try:
        for line in worker.stdout:
            item = json.loads(line)
            if item["kind"] == "center":
                MessageCenter().insert_system_message(item["title"], item["text"])
            elif item["kind"] == "render":
                try:
                    template = SandboxedEnvironment().from_string(item["template"])
                    body = template.render(title=item["title"], text=item["text"], image="", url="", user_id="")
                except Exception:
                    body = None
                    log.error("【Sites】统计通知模板渲染失败")
                worker.stdin.write(json.dumps({"body": body}, ensure_ascii=False) + "\n")
                worker.stdin.flush()
            elif item["kind"] == "done":
                if item["failed"]:
                    log.warn(f"【Sites】{item['failed']} 个站点未能完整刷新")
                return
            elif item["kind"] == "failed":
                log.warn("【Sites】本次定时统计未能完成")
                return
        raise ValueError("incomplete statistics refresh")
    finally:
        sites.init_favicons()


class StatisticsScheduler:
    """Connect the Go scheduler to the running client's site limiter and message center."""

    def __init__(self):
        self._stopped = Event()
        self._state = Lock()
        self._worker = None
        self._thread = Thread(target=self._serve, name="site-statistics", daemon=True)

    def start(self):
        self._thread.start()

    def stop(self):
        self._stopped.set()
        with self._state:
            worker = self._worker
            if worker is not None:
                try:
                    worker.stdin.close()
                except BrokenPipeError:
                    pass
        if worker is not None:
            try:
                worker.wait(timeout=10)
            except subprocess.TimeoutExpired:
                worker.kill()
                worker.wait()
        self._thread.join()

    def _serve(self):
        while not self._stopped.is_set():
            try:
                with self._state:
                    if self._stopped.is_set():
                        return
                    worker = subprocess.Popen([go_helper_path(), "site-statistics-scheduler"],
                                              stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                              text=True, encoding="utf-8")
                    self._worker = worker
                with worker:
                    for line in worker.stdout:
                        if json.loads(line).get("kind") != "targets":
                            raise ValueError("unexpected scheduler response")
                        while not self._stopped.is_set():
                            if lock.acquire(timeout=0.25):
                                try:
                                    _serve_refresh(worker)
                                finally:
                                    lock.release()
                                break
                        if self._stopped.is_set():
                            break
            except Exception:
                if not self._stopped.is_set():
                    log.error("【Sites】统计定时服务连接中断")
            finally:
                with self._state:
                    self._worker = None
            if not self._stopped.is_set():
                log.error("【Sites】统计定时服务已退出，5 秒后重试")
                self._stopped.wait(5)


@singleton
class SiteUserInfo(object):
    def refresh_site_data_now(self):
        self.refresh_statistics(summary=True)

    @staticmethod
    def refresh_statistics(specify_sites=None, summary=False):
        mode = "summary" if summary else "refresh"
        with lock:
            try:
                with subprocess.Popen([go_helper_path(), "site-statistics-run", mode],
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      text=True, encoding="utf-8") as worker:
                    _serve_refresh(worker, set(specify_sites or []))
                    if worker.wait():
                        raise ValueError("incomplete statistics refresh")
            except (OSError, ValueError, subprocess.SubprocessError):
                log.error("【Sites】统计刷新或消息投递未完成，请查看任务回执")

    def update_site_name(self, old_name, name):
        """
        更新站点数据中的站点名称
        """
        DbHelper().update_site_user_statistics_site_name(name, old_name)
        DbHelper().update_site_seed_info_site_name(name, old_name)
        DbHelper().update_site_statistics_site_name(name, old_name)
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