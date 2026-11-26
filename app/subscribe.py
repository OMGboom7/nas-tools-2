import json
from threading import Lock
from types import SimpleNamespace

import log
from app.conf import SystemConfig
from app.downloader import Downloader
from app.filter import Filter
from app.helper import DbHelper, MetaHelper
from app.indexer import Indexer
from app.media import Media, DouBan
from app.media.meta import MetaInfo
from app.message import Message
from app.plugins import EventManager
from app.searcher import Searcher
from app.sites import Sites
from app.utils import Torrent
from app.utils.commons import singleton
from app.utils.types import MediaType, SearchType, EventType, SystemConfigKey, RssType
from web.backend.web_utils import WebUtils
from config import Config, run_go_helper

lock = Lock()


def _progress(operation, **request):
    reply = run_go_helper("subscription-progress", operation, payload=json.dumps(request).encode(), timeout=60)
    return json.loads(reply)


def _progress_sources(kind, rid, state):
    for item in _progress("list", kind=kind, id=int(rid or 0), state=state or ""):
        row = SimpleNamespace(**item["Row"])
        ledger = item["Episodes"]
        row.progress = {"kind": kind, "id": row.ID, "token": item["Token"]}
        row.episodes = None
        if ledger:
            if len(ledger) != 1:
                log.warn(f"【Subscribe】订阅 {kind} {row.ID} 有多份缺集记录，跳过处理")
                continue
            row.episodes = [int(value) for value in (ledger[0]["EPISODES"] or "").split(",") if value]
        yield row


def _set_progress_state(source, state, **fields):
    result = _progress("state", **source, state=state, **fields)
    if result["Status"] != "updated":
        return False
    source["token"] = result["Token"]
    return True


@singleton
class Subscribe:
    dbhelper = None
    metahelper = None
    searcher = None
    message = None
    media = None
    downloader = None
    sites = None
    douban = None
    filter = None
    eventmanager = None
    indexer = None

    def __init__(self):
        self.init_config()

    def init_config(self):
        self.dbhelper = DbHelper()
        self.metahelper = MetaHelper()
        self.searcher = Searcher()
        self.message = Message()
        self.media = Media()
        self.downloader = Downloader()
        self.sites = Sites()
        self.douban = DouBan()
        self.indexer = Indexer()
        self.filter = Filter()
        self.eventmanager = EventManager()

    @property
    def default_rss_setting_tv(self):
        return SystemConfig().get(SystemConfigKey.DefaultRssSettingTV) or {}

    @property
    def default_rss_setting_mov(self):
        return SystemConfig().get(SystemConfigKey.DefaultRssSettingMOV) or {}

    def add_rss_subscribe(self, mtype, name, year,
                          channel=None,
                          keyword=None,
                          season=None,
                          fuzzy_match=False,
                          mediaid=None,
                          rss_sites=None,
                          search_sites=None,
                          over_edition=False,
                          filter_restype=None,
                          filter_pix=None,
                          filter_team=None,
                          filter_rule=None,
                          filter_include=None,
                          filter_exclude=None,
                          save_path=None,
                          download_setting=None,
                          total_ep=None,
                          current_ep=None,
                          state="D",
                          rssid=None,
                          in_from=None,
                          user_name=None,
                          chat_choice=None):
        """
        添加电影、电视剧订阅
        :param mtype: 类型，电影、电视剧、动漫
        :param name: 标题
        :param year: 年份，如要是剧集需要是首播年份
        :param channel: 自动或手动
        :param keyword: 自定义搜索词
        :param season: 第几季，数字
        :param fuzzy_match: 是否模糊匹配
        :param mediaid: 媒体ID，DB:/BG:/TMDBID
        :param rss_sites: 订阅站点列表，为空则表示全部站点
        :param search_sites: 搜索站点列表，为空则表示全部站点
        :param over_edition: 是否选版
        :param filter_restype: 质量过滤
        :param filter_pix: 分辨率过滤
        :param filter_team: 制作组/字幕组过滤
        :param filter_rule: 关键字过滤
        :param filter_include: 包含关键字
        :param filter_exclude: 排除关键字
        :param save_path: 保存路径
        :param download_setting: 下载设置
        :param state: 添加订阅时的状态
        :param rssid: 修改订阅时传入
        :param total_ep: 总集数
        :param current_ep: 开始订阅集数
        :param in_from: 来源
        :param user_name: 用户名
        :return: 错误码：0 代表成功，错误信息
        """
        if not name:
            return -1, "标题或类型有误", None
        year = int(year) if str(year).isdigit() else ""
        if isinstance(rss_sites, str):
            rss_sites = rss_sites.split(",")
        if isinstance(search_sites, str):
            search_sites = search_sites.split(",")
        filter_rule = int(filter_rule) if str(filter_rule).isdigit() else None
        total_ep = int(total_ep) if str(total_ep).isdigit() else None
        current_ep = int(current_ep) if str(current_ep).isdigit() else None
        download_setting = int(download_setting) if str(download_setting).removeprefix("-").isdecimal() else ""

        if fuzzy_match:
            media_info = MetaInfo(title=name, mtype=mtype)
            media_info.title, media_info.type = name, mtype
            if season:
                media_info.begin_season = int(season)
        else:
            if mediaid:
                media_info = WebUtils.get_mediainfo_from_id(mtype=mtype, mediaid=mediaid, wait=True)
                if media_info and not season:
                    season = media_info.begin_season
            else:
                title = f"{name} {year} 第{season}季" if season else f"{name} {year}"
                media_info = self.media.get_media_info(title=title, mtype=mtype, strict=bool(year), cache=False)
            if not media_info or not media_info.tmdb_info:
                return 1, "无法TMDB查询到媒体信息", None

        kind = "MOV" if media_info.type == MediaType.MOVIE else "TV"
        default_kind = ("TV" if mtype == MediaType.TV else "MOV") if channel == RssType.Auto else ""
        receipt = self.dbhelper.save_rss_subscription(
            media_info, kind, state="R" if fuzzy_match else state, rss_sites=rss_sites, search_sites=search_sites,
            over_edition=int(bool(over_edition)), filter_restype=filter_restype, filter_pix=filter_pix,
            filter_team=filter_team, filter_rule=filter_rule, filter_include=filter_include,
            filter_exclude=filter_exclude, save_path=save_path, download_setting=download_setting,
            fuzzy_match=fuzzy_match, desc=None if fuzzy_match else media_info.overview,
            note=None if fuzzy_match else self.gen_rss_note(media_info), keyword=keyword,
            total_ep=None if fuzzy_match else total_ep, current_ep=None if fuzzy_match else current_ep,
            rssid=rssid, default_kind=default_kind, choose_season=(kind == "TV" and not fuzzy_match),
            season=int(season) if kind == "TV" and season is not None and season != "" else None,
            douban=str(mediaid).startswith("DB:"), choice=chat_choice)
        if receipt.get("season") is not None:
            media_info.begin_season = receipt["season"]
            media_info.total_episodes = receipt["episodes"]
        if receipt["code"] == 0:
            event = receipt["event"]
            event.update(media=media_info.to_dict(), rssid=rssid, total_ep=total_ep, current_ep=current_ep)
            self.eventmanager.send_event(EventType.SubscribeAdd, event)
            if in_from:
                media_info.user_name = user_name
                self.message.send_rss_success_message(in_from=in_from, media_info=media_info)
        return receipt["code"], receipt["message"], media_info

    def finish_progress(self, source, media, operation="finish", **fields):
        domain = Config().get_domain()
        notice = {"title": media.get_title_string(), "season": media.get_season_string(),
                  "type": media.type.value, "vote": media.get_vote_string() if media.vote_average else "",
                  "image": media.get_message_image() if source["kind"] == "TV" else "", "link": f"{domain}?next=downloaded" if domain else ""}
        result = _progress(operation, **source, **fields, poster=media.get_poster_image(),
                           overview=media.overview, notice=notice)
        if result["Status"] != "finished":
            return False
        self.eventmanager.send_event(EventType.SubscribeFinished, {"media_info": media.to_dict(), "rssid": source["id"]})
        log.info(f"【Subscribe】{notice['title']} {notice['season']} 订阅完成")
        return True

    def get_subscribe_movies(self, rid=None, state=None, progress=False):
        """
        获取电影订阅
        """
        ret_dict = {}
        rss_movies = (_progress_sources("MOV", rid, state) if progress else
                     self.dbhelper.get_rss_movies(rssid=rid, state=state))
        rss_sites_valid = self.sites.get_site_names(rss=True)
        search_sites_valid = self.indexer.get_user_indexer_names()
        for rss_movie in rss_movies:
            desc = rss_movie.DESC
            note = rss_movie.NOTE
            tmdbid = rss_movie.TMDBID
            rss_sites = json.loads(rss_movie.RSS_SITES) if rss_movie.RSS_SITES else []
            search_sites = json.loads(rss_movie.SEARCH_SITES) if rss_movie.SEARCH_SITES else []
            over_edition = True if rss_movie.OVER_EDITION == 1 else False
            filter_restype = rss_movie.FILTER_RESTYPE
            filter_pix = rss_movie.FILTER_PIX
            filter_team = rss_movie.FILTER_TEAM
            filter_rule = rss_movie.FILTER_RULE
            filter_include = rss_movie.FILTER_INCLUDE
            filter_exclude = rss_movie.FILTER_EXCLUDE
            download_setting = rss_movie.DOWNLOAD_SETTING
            save_path = rss_movie.SAVE_PATH
            fuzzy_match = True if rss_movie.FUZZY_MATCH == 1 else False
            keyword = rss_movie.KEYWORD
            # 兼容旧配置
            if desc and desc.find('{') != -1:
                desc = self.__parse_rss_desc(desc)
                rss_sites = desc.get("rss_sites")
                search_sites = desc.get("search_sites")
                over_edition = True if desc.get("over_edition") == 'Y' else False
                filter_restype = desc.get("restype")
                filter_pix = desc.get("pix")
                filter_team = desc.get("team")
                filter_rule = desc.get("rule")
                download_setting = ""
                save_path = ""
                fuzzy_match = False if tmdbid else True
            if note:
                note_info = self.__parse_rss_desc(note)
            else:
                note_info = {}
            rss_sites = [site for site in rss_sites if site in rss_sites_valid]
            search_sites = [site for site in search_sites if site in search_sites_valid]
            ret_dict[str(rss_movie.ID)] = {
                "id": rss_movie.ID,
                "name": rss_movie.NAME,
                "year": rss_movie.YEAR,
                "tmdbid": rss_movie.TMDBID,
                "image": rss_movie.IMAGE,
                "overview": rss_movie.DESC,
                "rss_sites": rss_sites,
                "search_sites": search_sites,
                "over_edition": over_edition,
                "filter_restype": filter_restype,
                "filter_pix": filter_pix,
                "filter_team": filter_team,
                "filter_rule": filter_rule,
                "filter_include": filter_include,
                "filter_exclude": filter_exclude,
                "save_path": save_path,
                "download_setting": download_setting,
                "fuzzy_match": fuzzy_match,
                "state": rss_movie.STATE,
                "poster": note_info.get("poster"),
                "release_date": note_info.get("release_date"),
                "vote": note_info.get("vote"),
                "keyword": keyword

            }
            if progress:
                ret_dict[str(rss_movie.ID)].update(progress=rss_movie.progress, episodes=rss_movie.episodes, rank=rss_movie.FILTER_ORDER or 0)
        return ret_dict

    def get_subscribe_tvs(self, rid=None, state=None, progress=False):
        ret_dict = {}
        rss_tvs = (_progress_sources("TV", rid, state) if progress else
                     self.dbhelper.get_rss_tvs(rssid=rid, state=state))
        rss_sites_valid = self.sites.get_site_names(rss=True)
        search_sites_valid = self.indexer.get_user_indexer_names()
        for rss_tv in rss_tvs:
            desc = rss_tv.DESC
            note = rss_tv.NOTE
            tmdbid = rss_tv.TMDBID
            rss_sites = json.loads(rss_tv.RSS_SITES) if rss_tv.RSS_SITES else []
            search_sites = json.loads(rss_tv.SEARCH_SITES) if rss_tv.SEARCH_SITES else []
            over_edition = True if rss_tv.OVER_EDITION == 1 else False
            filter_restype = rss_tv.FILTER_RESTYPE
            filter_pix = rss_tv.FILTER_PIX
            filter_team = rss_tv.FILTER_TEAM
            filter_rule = rss_tv.FILTER_RULE
            filter_include = rss_tv.FILTER_INCLUDE
            filter_exclude = rss_tv.FILTER_EXCLUDE
            download_setting = rss_tv.DOWNLOAD_SETTING
            save_path = rss_tv.SAVE_PATH
            total_ep = rss_tv.TOTAL_EP
            current_ep = rss_tv.CURRENT_EP
            fuzzy_match = True if rss_tv.FUZZY_MATCH == 1 else False
            keyword = rss_tv.KEYWORD
            # 兼容旧配置
            if desc and desc.find('{') != -1:
                desc = self.__parse_rss_desc(desc)
                rss_sites = desc.get("rss_sites")
                search_sites = desc.get("search_sites")
                over_edition = True if desc.get("over_edition") == 'Y' else False
                filter_restype = desc.get("restype")
                filter_pix = desc.get("pix")
                filter_team = desc.get("team")
                filter_rule = desc.get("rule")
                filter_include = desc.get("include")
                filter_exclude = desc.get("exclude")
                save_path = ""
                download_setting = ""
                total_ep = desc.get("total")
                current_ep = desc.get("current")
                fuzzy_match = False if tmdbid else True
            if note:
                note_info = self.__parse_rss_desc(note)
            else:
                note_info = {}
            rss_sites = [site for site in rss_sites if site in rss_sites_valid]
            search_sites = [site for site in search_sites if site in search_sites_valid]
            ret_dict[str(rss_tv.ID)] = {
                "id": rss_tv.ID,
                "name": rss_tv.NAME,
                "year": rss_tv.YEAR,
                "season": rss_tv.SEASON,
                "tmdbid": rss_tv.TMDBID,
                "image": rss_tv.IMAGE,
                "overview": rss_tv.DESC,
                "rss_sites": rss_sites,
                "search_sites": search_sites,
                "over_edition": over_edition,
                "filter_restype": filter_restype,
                "filter_pix": filter_pix,
                "filter_team": filter_team,
                "filter_rule": filter_rule,
                "filter_include": filter_include,
                "filter_exclude": filter_exclude,
                "save_path": save_path,
                "download_setting": download_setting,
                "total": rss_tv.TOTAL,
                "lack": rss_tv.LACK,
                "total_ep": total_ep,
                "current_ep": current_ep,
                "fuzzy_match": fuzzy_match,
                "state": rss_tv.STATE,
                "poster": note_info.get("poster"),
                "release_date": note_info.get("release_date"),
                "vote": note_info.get("vote"),
                "keyword": keyword
            }
            if progress:
                ret_dict[str(rss_tv.ID)].update(progress=rss_tv.progress, episodes=rss_tv.episodes, rank=rss_tv.FILTER_ORDER or 0)
        return ret_dict

    @staticmethod
    def __parse_rss_desc(desc):
        """
        解析订阅的JSON字段
        """
        if not desc:
            return {}
        return json.loads(desc) or {}

    @staticmethod
    def gen_rss_note(media):
        """
        生成订阅的JSON备注信息
        :param media: 媒体信息
        :return: 备注信息
        """
        if not media:
            return {}
        note = {
            "poster": media.get_poster_image(),
            "release_date": media.release_date,
            "vote": media.vote_average
        }
        return json.dumps(note)

    def refresh_rss_metainfo(self):
        """
        定时将豆瓣订阅转换为TMDB的订阅，并更新订阅的TMDB信息
        """
        # 更新电影
        log.info("【Subscribe】开始刷新订阅TMDB信息...")
        name_follow_tmdb_changed = True
        media = Config().get_config('media')
        if media:
            name_follow_tmdb_changed = media.get("name_follow_tmdb_changed", True) or True
        rss_movies = self.get_subscribe_movies(state='R')
        for rid, rss_info in rss_movies.items():
            # 跳过模糊匹配的
            if rss_info.get("fuzzy_match"):
                continue
            rssid = rss_info.get("id")
            name = rss_info.get("name")
            year = rss_info.get("year") or ""
            tmdbid = rss_info.get("tmdbid")
            # 更新TMDB信息
            media_info = self.__get_media_info(tmdbid=tmdbid,
                                               name=name,
                                               year=year,
                                               mtype=MediaType.MOVIE,
                                               cache=False)
            if media_info and media_info.tmdb_id and media_info.title != name and name_follow_tmdb_changed:
                log.info(f"【Subscribe】检测到TMDB信息变化，更新电影订阅 {name} 为 {media_info.title}")
                # 更新订阅信息
                self.dbhelper.update_rss_movie_tmdb(rid=rssid,
                                                    tmdbid=media_info.tmdb_id,
                                                    title=media_info.title,
                                                    year=media_info.year,
                                                    image=media_info.get_message_image(),
                                                    desc=media_info.overview,
                                                    note=self.gen_rss_note(media_info))
                # 清除TMDB缓存
                self.metahelper.delete_meta_data_by_tmdbid(media_info.tmdb_id)

        # 更新电视剧
        rss_tvs = self.get_subscribe_tvs(state='R')
        for rid, rss_info in rss_tvs.items():
            # 跳过模糊匹配的
            if rss_info.get("fuzzy_match"):
                continue
            rssid = rss_info.get("id")
            name = rss_info.get("name")
            year = rss_info.get("year") or ""
            tmdbid = rss_info.get("tmdbid")
            season = rss_info.get("season") or 1
            total = rss_info.get("total")
            total_ep = rss_info.get("total_ep")
            lack = rss_info.get("lack")
            # 更新TMDB信息
            media_info = self.__get_media_info(tmdbid=tmdbid,
                                               name=name,
                                               year=year,
                                               mtype=MediaType.TV,
                                               cache=False)
            if media_info and media_info.tmdb_id:
                # 获取总集数
                total_episode = self.media.get_tmdb_season_episodes_num(tv_info=media_info.tmdb_info,
                                                                        season=int(str(season).replace("S", "")))
                # 设置总集数的，不更新集数
                if total_ep:
                    total_episode = total_ep
                if total_episode and (name != media_info.title or total != total_episode) and name_follow_tmdb_changed:
                    # 新的缺失集数
                    lack_episode = total_episode - (total - lack)
                    log.info(
                        f"【Subscribe】检测到TMDB信息变化，更新电视剧订阅 {name} 为 {media_info.title}，总集数为：{total_episode}")
                    # 更新订阅信息
                    self.dbhelper.update_rss_tv_tmdb(rid=rssid,
                                                     tmdbid=media_info.tmdb_id,
                                                     title=media_info.title,
                                                     year=media_info.year,
                                                     total=total_episode,
                                                     lack=lack_episode,
                                                     image=media_info.get_message_image(),
                                                     desc=media_info.overview,
                                                     note=self.gen_rss_note(media_info))
                    # 更新缺失季集
                    self.dbhelper.update_rss_tv_episodes(
                        rid=rssid, 
                        episodes=range(total_episode - lack_episode + 1, total_episode + 1)
                    )
                    # 清除TMDB缓存
                    self.metahelper.delete_meta_data_by_tmdbid(media_info.tmdb_id)
        log.info("【Subscribe】订阅TMDB信息刷新完成")

    def __get_media_info(self, tmdbid, name, year, mtype, cache=True):
        """
        综合返回媒体信息
        """
        if tmdbid and not str(tmdbid).startswith("DB:"):
            media_info = MetaInfo(title="%s %s".strip() % (name, year))
            tmdb_info = self.media.get_tmdb_info(mtype=mtype, tmdbid=tmdbid)
            media_info.set_tmdb_info(tmdb_info)
        else:
            media_info = self.media.get_media_info(title="%s %s" % (name, year), mtype=mtype, strict=True, cache=cache)
        return media_info

    def subscribe_search_all(self):
        """
        搜索R状态的所有订阅，由定时服务调用
        """
        self.subscribe_search(state="R")

    def subscribe_search(self, state="D"):
        """
        RSS订阅队列中状态的任务处理，先进行存量资源搜索，缺失的才标志为RSS状态，由定时服务调用
        """
        try:
            lock.acquire()
            # 处理电影
            self.subscribe_search_movie(state=state)
            # 处理电视剧
            self.subscribe_search_tv(state=state)
        finally:
            lock.release()

    def subscribe_search_movie(self, rssid=None, state='D'):
        """
        搜索电影RSS
        :param rssid: 订阅ID，未输入时搜索所有状态为D的，输入时搜索该ID任何状态的
        :param state: 搜索的状态，默认为队列中才搜索
        """
        if rssid:
            rss_movies = self.get_subscribe_movies(rid=rssid, progress=True)
        else:
            rss_movies = self.get_subscribe_movies(state=state, progress=True)
        if rss_movies:
            log.info("【Subscribe】共有 %s 个电影订阅需要搜索" % len(rss_movies))
        for rid, rss_info in rss_movies.items():
            # 跳过模糊匹配的
            if rss_info.get("fuzzy_match"):
                continue
            # 搜索站点范围
            rssid = rss_info.get("id")
            name = rss_info.get("name")
            year = rss_info.get("year") or ""
            tmdbid = rss_info.get("tmdbid")
            over_edition = rss_info.get("over_edition")
            keyword = rss_info.get("keyword")

            # 开始搜索
            source = rss_info["progress"]
            if not _set_progress_state(source, 'S'):
                continue

            try:
                # 识别
                media_info = self.__get_media_info(tmdbid, name, year, MediaType.MOVIE)
                # 未识别到媒体信息
                if not media_info or not media_info.tmdb_info:
                    _set_progress_state(source, 'R')
                    continue
                media_info.set_download_info(download_setting=rss_info.get("download_setting"),
                                             save_path=rss_info.get("save_path"))
                # 自定义搜索词
                media_info.keyword = keyword
                media_info.rssid = rssid
                # 非洗版的情况检查是否存在
                if not over_edition:
                    # 检查是否存在
                    exist_flag, no_exists, _ = self.downloader.check_exists_medias(meta_info=media_info)
                    if exist_flag is None:
                        _set_progress_state(source, 'R')
                        continue
                    # 已经存在
                    if exist_flag:
                        log.info("【Subscribe】电影 %s 已存在" % media_info.get_title_string())
                        self.finish_progress(source, media_info)
                        continue
                else:
                    # 洗版时按缺失来下载
                    no_exists = {}
                    # 把洗版标志加入搜索
                    media_info.over_edition = over_edition
                    # 将当前的优先级传入搜索
                    media_info.res_order = rss_info["rank"]
                # 开始搜索
                filter_dict = {
                    "restype": rss_info.get('filter_restype'),
                    "pix": rss_info.get('filter_pix'),
                    "team": rss_info.get('filter_team'),
                    "rule": rss_info.get('filter_rule'),
                    "include": rss_info.get('filter_include'),
                    "exclude": rss_info.get('filter_exclude'),
                    "site": rss_info.get("search_sites")
                }
                search_result, _, _, _ = self.searcher.search_one_media(
                    media_info=media_info,
                    in_from=SearchType.RSS,
                    no_exists=no_exists,
                    sites=rss_info.get("search_sites"),
                    filters=filter_dict)
                if search_result:
                    # 洗版
                    if over_edition:
                        self.edition_progress(source, search_result)
                    else:
                        self.finish_progress(source, media_info)
                else:
                    _set_progress_state(source, 'R')
            except Exception as err:
                _set_progress_state(source, 'R')
                log.error(f"【Subscribe】电影 {name} 订阅搜索失败：{str(err)}")
                continue

    def subscribe_search_tv(self, rssid=None, state="D"):
        """
        搜索电视剧RSS
        :param rssid: 订阅ID，未输入时搜索所有状态为D的，输入时检索该ID任何状态的
        :param state: 检索的状态，默认为队列中才检索
        """
        if rssid:
            rss_tvs = self.get_subscribe_tvs(rid=rssid, progress=True)
        else:
            rss_tvs = self.get_subscribe_tvs(state=state, progress=True)
        if rss_tvs:
            log.info("【Subscribe】共有 %s 个电视剧订阅需要检索" % len(rss_tvs))
        for rid, rss_info in rss_tvs.items():
            # 跳过模糊匹配的
            if rss_info.get("fuzzy_match"):
                continue
            rssid = rss_info.get("id")
            name = rss_info.get("name")
            year = rss_info.get("year") or ""
            tmdbid = rss_info.get("tmdbid")
            over_edition = rss_info.get("over_edition")
            keyword = rss_info.get("keyword")

            # 开始搜索
            source = rss_info["progress"]
            if not _set_progress_state(source, 'S'):
                continue

            try:
                # 识别
                media_info = self.__get_media_info(tmdbid, name, year, MediaType.TV)
                # 未识别到媒体信息
                if not media_info or not media_info.tmdb_info:
                    _set_progress_state(source, 'R')
                    continue
                # 取下载设置
                media_info.set_download_info(download_setting=rss_info.get("download_setting"),
                                             save_path=rss_info.get("save_path"))
                # 从登记薄中获取缺失剧集
                season = 1
                if rss_info.get("season") not in (None, ""):
                    season = int(str(rss_info.get("season")).replace("S", ""))
                # 订阅季
                media_info.begin_season = season
                # 订阅ID
                media_info.rssid = rssid
                # 自定义集数
                total_ep = rss_info.get("total")
                current_ep = rss_info.get("current_ep")
                # 自定义搜索词
                media_info.keyword = keyword
                # 表中记录的剩余订阅集数
                episodes = rss_info["episodes"]
                if episodes is None:
                    episodes = list(range(int(current_ep), int(total_ep) + 1)) if current_ep else []
                rss_no_exists = {media_info.tmdb_id: [
                    {"season": season, "episodes": episodes, "total_episodes": total_ep}
                ]}
                # 非洗版时检查本地媒体库情况
                if not over_edition:
                    exist_flag, library_no_exists, _ = self.downloader.check_exists_medias(
                        meta_info=media_info,
                        total_ep={season: total_ep})
                    if exist_flag is None:
                        _set_progress_state(source, 'R')
                        continue
                    # 当前剧集已存在，跳过
                    if exist_flag:
                        # 已全部存在
                        if not library_no_exists \
                                or not library_no_exists.get(media_info.tmdb_id):
                            log.info("【Subscribe】电视剧 %s 订阅剧集已全部存在" % (
                                media_info.get_title_string()))
                            # 完成订阅
                            self.finish_progress(source, media_info)
                        continue
                    # 取交集做为缺失集
                    rss_no_exists = Torrent.get_intersection_episodes(target=rss_no_exists,
                                                                      source=library_no_exists,
                                                                      title=media_info.tmdb_id)
                    if not rss_no_exists.get(media_info.tmdb_id):
                        self.finish_progress(source, media_info)
                        continue
                    log.info("【Subscribe】%s 订阅缺失季集：%s" % (
                        media_info.get_title_string(), rss_no_exists[media_info.tmdb_id]))
                else:
                    # 把洗版标志加入检索
                    media_info.over_edition = over_edition
                    # 将当前的优先级传入检索
                    media_info.res_order = rss_info["rank"]
                # 开始检索
                filter_dict = {
                    "restype": rss_info.get('filter_restype'),
                    "pix": rss_info.get('filter_pix'),
                    "team": rss_info.get('filter_team'),
                    "rule": rss_info.get('filter_rule'),
                    "include": rss_info.get('filter_include'),
                    "exclude": rss_info.get('filter_exclude'),
                    "site": rss_info.get("search_sites")
                }
                search_result, no_exists, _, _ = self.searcher.search_one_media(
                    media_info=media_info,
                    in_from=SearchType.RSS,
                    no_exists=rss_no_exists,
                    sites=rss_info.get("search_sites"),
                    filters=filter_dict)
                if search_result:
                    if over_edition:
                        self.edition_progress(source, search_result)
                    else:
                        self.finish_progress(source, media_info)
                elif over_edition:
                    _set_progress_state(source, 'R')
                else:
                    self.remaining_progress(source, season, (no_exists or {}).get(media_info.tmdb_id, []))
            except Exception as err:
                log.error(f"【Subscribe】电视剧 {name} 订阅搜索失败：{str(err)}")
                _set_progress_state(source, 'R')
                continue

    def edition_progress(self, source, media, **fields):
        if not media or not media.res_order or not media.filter_rule or media.get_episode_list():
            return _set_progress_state(source, 'R', **fields)
        highest = self.filter.get_rule_first_order(rulegroup=media.filter_rule)
        if highest is None:
            return _set_progress_state(source, 'R', **fields)
        return self.finish_progress(source, media, "edition", rank=int(media.res_order), highest=int(highest), **fields)

    @staticmethod
    def remaining_progress(source, season, rows, **fields):
        for row in rows:
            if int(row["season"]) == season and row.get("episodes"):
                return _progress("remaining", **source, episodes=[int(ep) for ep in row["episodes"]], **fields)
        return _set_progress_state(source, 'R', **fields)

    @staticmethod
    def rss_download_markers(items):
        return [{"name": item.org_string, "enclosure": item.enclosure, "type": item.type.value,
                 "title": item.title, "year": str(item.year or ""), "season": item.get_season_string(),
                 "episode": item.get_episode_string()} for item in items]

    def record_rss_downloads(self, items):
        return _progress("record", kind="TV" if items[0].type != MediaType.MOVIE else "MOV",
                         markers=self.rss_download_markers(items))

    def check_history(self, type_str, name, year, season):
        """
        检查订阅历史是否存在
        """
        return self.dbhelper.check_rss_history(type_str=type_str,
                                               name=name,
                                               year=year,
                                               season=season)

    def delete_subscribe(self, mtype,
                         title=None, year=None, season=None, rssid=None, tmdbid=None):
        """
        删除电影订阅
        """
        if mtype == MediaType.MOVIE:
            return self.dbhelper.delete_rss_movie(title=title, year=year, rssid=rssid, tmdbid=tmdbid)
        else:
            return self.dbhelper.delete_rss_tv(title=title, season=season, rssid=rssid, tmdbid=tmdbid)

    def get_subscribe_id(self, mtype,
                         title, year=None, season=None, tmdbid=None):
        """
        获取订阅ID
        """
        if mtype == MediaType.MOVIE:
            return self.dbhelper.get_rss_movie_id(title=title,
                                                  year=year,
                                                  tmdbid=tmdbid)
        else:
            return self.dbhelper.get_rss_tv_id(title=title,
                                               year=year,
                                               season=season,
                                               tmdbid=tmdbid)
