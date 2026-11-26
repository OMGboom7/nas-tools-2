import json
import subprocess
from threading import Lock

import log
from app.downloader import Downloader
from app.filter import Filter, resource_filter_batch
from app.helper import DbHelper
from app.media import Media
from app.media.meta import MetaInfo
from app.message import Message
from app.sites import Sites, SiteConf
from app.subscribe import Subscribe
from app.utils import ExceptionUtils, Torrent
from app.utils.commons import singleton
from app.utils.types import EventType, MediaType, SearchType
from config import go_helper_path

lock = Lock()


@singleton
class Rss:
    filter = None
    media = None
    sites = None
    siteconf = None
    downloader = None
    searcher = None
    dbhelper = None
    subscribe = None
    message = None

    def __init__(self):
        self.init_config()

    def init_config(self):
        self.media = Media()
        self.downloader = Downloader()
        self.sites = Sites()
        self.siteconf = SiteConf()
        self.filter = Filter()
        self.dbhelper = DbHelper()
        self.subscribe = Subscribe()
        self.message = Message()

    @resource_filter_batch()
    def rssdownload(self):
        """
        RSS订阅搜索下载入口，由定时服务调用
        """
        with lock, subprocess.Popen([go_helper_path(), "rss-scan"], stdin=subprocess.PIPE,
                                    stdout=subprocess.PIPE, text=True, encoding="utf-8") as scan:
            accepted = {}
            media = None
            try:
                for line in scan.stdout:
                    frame = json.loads(line)
                    op = frame["op"]
                    if op == "done":
                        if scan.wait() != 0:
                            raise RuntimeError("RSS 扫描退出异常")
                        finished = frame.get("finished") or {}
                        for item, _ in accepted.values():
                            source = getattr(item, "ordinary_source", None)
                            if source and finished.get(source["token"]) == item.enclosure:
                                finished.pop(source["token"])
                                self.subscribe.eventmanager.send_event(EventType.SubscribeFinished,
                                                                      {"media_info": item.to_dict(), "rssid": source["id"]})
                        return
                    if op in ("expired", "failed"):
                        name = frame["site"]
                        log.warn(f"【Rss】站点 {name} 读取失败")
                        if op == "expired":
                            self.message.send_site_message(title="【RSS链接过期提醒】",
                                                           text=f"站点：{name}，请在站点设置中更新 RSS 链接。")
                        continue
                    index = frame["index"]
                    reply = {"op": op, "index": index}
                    if op == "identify":
                        media = None
                        try:
                            media = self._identify(frame)
                            if media:
                                reply["identity"] = {
                                    "kind": "MOV" if media.type == MediaType.MOVIE else "TV",
                                    "title": str(media.title or ""), "year": str(media.year or ""),
                                    "tmdbid": str(media.tmdb_id or ""), "season": str(media.get_season_string() or ""),
                                    "revised": str(media.rev_string or ""),
                                }
                        except Exception as e:
                            ExceptionUtils.exception_traceback(e)
                    elif op == "candidate":
                        try:
                            gaps = self._candidate(media, frame["site"], frame["source"])
                            if gaps is not None:
                                accepted[index] = (media, gaps)
                                reply["accepted"] = True
                        except Exception as e:
                            ExceptionUtils.exception_traceback(e)
                    elif op == "submit":
                        items, missing = [], {}
                        for selected in frame["indices"]:
                            item, gaps = accepted[selected]
                            if item not in items:
                                items.append(item)
                            for tmdbid, seasons in gaps.items():
                                by_season = missing.setdefault(tmdbid, {})
                                for season in seasons:
                                    by_season[season["season"]] = season
                        for item in items:
                            item.ordinary_receipt = self._ordinary_receipt(item, missing.get(item.tmdb_id, {}))
                        self.download_rss_torrent(items, {k: list(v.values()) for k, v in missing.items()})
                    else:
                        raise ValueError("未知 RSS 扫描指令")
                    scan.stdin.write(json.dumps(reply) + "\n")
                    scan.stdin.flush()
                raise RuntimeError("RSS 扫描中断")
            finally:
                if scan.poll() is None:
                    scan.terminate()

    def _identify(self, frame):
        article = frame["article"]
        media = MetaInfo(title=article["title"])
        cached = self.media.get_cache_info(media)
        if cached.get("id"):
            media.tmdb_id = cached["id"]
            media.type = cached.get("type")
            media.title = cached.get("title")
            media.year = cached.get("year")
        else:
            media = self.media.get_media_info(title=article["title"], parsed=media, cache_info=cached)
        if media:
            media.set_torrent_info(size=article["size"], page_url=article.get("link"),
                                   site=frame["site"], site_order=frame["order"], enclosure=article["enclosure"])
        return media

    def _candidate(self, media, site, source):
        upload = download = None
        if site["parse"]:
            if self.sites.check_ratelimit(site["id"]):
                return None
            attrs = self.siteconf.check_torrent_attr(torrent_url=media.page_url, cookie=site["cookie"],
                                                     ua=site["ua"], proxy=site["proxy"])
            upload = 2.0 if attrs.get("2xfree") else 1.0
            download = 0.0 if attrs.get("2xfree") or attrs.get("free") else 1.0
            media.set_torrent_info(upload_volume_factor=upload, download_volume_factor=download,
                                   hit_and_run=bool(attrs.get("hr")))
        rule = source["filter_rule"] or site["rule"]
        filters = {key: source["filter_" + key] for key in ("restype", "pix", "team", "include", "exclude")}
        filters["rule"] = rule
        match, rank, _ = self.filter.check_torrent_filter(meta_info=media, filter_args=filters)
        if not match:
            return None
        gaps = {}
        media.ordinary_source = source["progress"]
        media.subscription_progress = None
        if not source["fuzzy_match"]:
            if not media.tmdb_info and media.tmdb_id:
                media.set_tmdb_info(self.media.get_tmdb_info(mtype=media.type, tmdbid=media.tmdb_id))
            if not media.tmdb_info:
                return None
            season = int(str(source["season"]).lstrip("S")) if source["season"] not in (None, "") else 1
            if source["over_edition"]:
                if source["rank"] and int(source["rank"]) >= int(rank or 0):
                    return None
            elif media.type == MediaType.MOVIE:
                exists, gaps, _ = self.downloader.check_exists_medias(meta_info=media)
                if exists is not False:
                    return None
            else:
                total, episodes = source["total"], source["episodes"]
                if episodes is None:
                    episodes = list(range(int(source["current_ep"]), int(total) + 1)) if source["current_ep"] else []
                exists, library, _ = self.downloader.check_exists_medias(meta_info=media, total_ep={season: total})
                if exists is not False:
                    return None
                wanted = {media.tmdb_id: [{"season": season, "episodes": episodes, "total_episodes": total}]}
                gaps = Torrent.get_intersection_episodes(wanted, library, media.tmdb_id)
                if not gaps:
                    return None
            media.subscription_progress = source["progress"]
            media.subscription_season = season
        if self.sites.check_ratelimit(site["id"]):
            return None
        media.set_torrent_info(res_order=rank, filter_rule=rule, over_edition=source["over_edition"],
                               download_volume_factor=download, upload_volume_factor=upload, rssid=source["id"])
        media.set_download_info(download_setting=source["download_setting"], save_path=source["save_path"])
        return gaps

    def _ordinary_receipt(self, media, gaps):
        source = media.ordinary_source
        receipt = {**source, "mode": "record", "marker": self.subscribe.rss_download_markers([media])[0]}
        if not media.subscription_progress:
            return receipt
        season = media.subscription_season
        receipt["season"] = season
        if media.over_edition:
            if not media.filter_rule or not media.res_order or media.get_episode_list():
                return receipt
            highest = self.filter.get_rule_first_order(rulegroup=media.filter_rule)
            if highest is None:
                receipt.update(mode="state", rank=int(media.res_order))
                return receipt
            receipt.update(mode="edition", rank=int(media.res_order), highest=int(highest))
        elif source["kind"] == "TV":
            row = gaps[season]
            missing = row["episodes"] or range(1, int(row["total_episodes"]) + 1)
            receipt.update(mode="episodes", missing=sorted(int(n) for n in missing),
                           covered=media.get_episode_list())
        else:
            receipt["mode"] = "finish"
        receipt.update(self.subscribe.completion_fields(media, source["kind"]))
        return receipt

    def download_rss_torrent(self, rss_download_torrents, rss_no_exists):
        """
        根据缺失情况以及匹配到的结果选择下载种子
        """

        if not rss_download_torrents:
            return

        # 去重择优后开始添加下载
        download_items, left_medias = self.downloader.batch_download(SearchType.RSS,
                                                                     rss_download_torrents,
                                                                     rss_no_exists)
        # 批量删除订阅
        if download_items:
            groups = {}
            unscoped = []
            for item in download_items:
                source = getattr(item, "subscription_progress", None)
                if source:
                    groups.setdefault((source["kind"], source["id"]), []).append(item)
                elif not getattr(item, "subscription_recorded", False):
                    unscoped.append(item)
            if unscoped:
                self.subscribe.record_rss_downloads(unscoped)
            for items in groups.values():
                if all(getattr(item, "subscription_recorded", False) for item in items):
                    continue
                item = items[0]
                source = item.subscription_progress
                markers = self.subscribe.rss_download_markers(items)
                if item.over_edition:
                    whole = [entry for entry in items if not entry.get_episode_list() and entry.filter_rule and entry.res_order]
                    if whole:
                        self.subscribe.edition_progress(source, max(whole, key=lambda entry: int(entry.res_order)), markers=markers)
                    else:
                        self.subscribe.record_rss_downloads(items)
                else:
                    season = item.subscription_season
                    gaps = (left_medias or {}).get(item.tmdb_id, [])
                    if source["kind"] == "TV" and any(int(row["season"]) == season for row in gaps):
                        self.subscribe.remaining_progress(source, season, gaps, markers=markers)
                    else:
                        self.subscribe.finish_progress(source, item, markers=markers)
            log.info("【Rss】实际下载了 %s 个资源" % len(download_items))
        else:
            log.info("【Rss】未下载到任何资源")

    def delete_rss_history(self, rssid):
        """
        删除订阅历史
        """
        self.dbhelper.delete_rss_history(rssid=rssid)

    def get_rss_history(self, rtype=None, rid=None):
        """
        获取订阅历史
        """
        return self.dbhelper.get_rss_history(rtype=rtype, rid=rid)
