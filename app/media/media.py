import hashlib
import json
import subprocess
import os
import random
import traceback
from threading import RLock

from cachetools import TTLCache, cached

import zhconv

import log
from app.helper import MetaHelper
from app.media.meta.metainfo import MetaInfo
from app.media.tmdbv3api import TMDb, Search, Movie, TV, Person, Find, TMDbException, Discover, Trending, Episode, Genre
from app.media.tmdbv3api.as_obj import AsObj
from app.utils import PathUtils, EpisodeFormat, NumberUtils, StringUtils
from app.utils.types import MediaType
from config import Config, run_go_helper


@cached(cache=TTLCache(maxsize=512, ttl=6 * 60 * 60), lock=RLock())
def _tmdb_detail_bytes(payload, config_revision, recognize=False):
    # A config change (including provider, language, key or proxy) invalidates the entry.
    if recognize:
        return run_go_helper("search-titles", "recognize", payload=payload, timeout=155)
    return run_go_helper("tmdb-detail", payload=payload, timeout=25)


def _tmdb_query(query, recognize=False):
    with open(os.environ["NASTOOL_CONFIG"], "rb") as config_file:
        revision = hashlib.sha256(config_file.read()).digest()
    return json.loads(_tmdb_detail_bytes(json.dumps(query).encode(), revision, recognize))


def _tmdb_detail(query):
    result = _tmdb_query(query)
    return AsObj(**result) if result is not None else None


class Media:
    # TheMovieDB
    tmdb = None
    search = None
    movie = None
    tv = None
    episode = None
    person = None
    find = None
    trending = None
    discover = None
    genre = None
    meta = None
    _default_language = None
    _tmdb_include_adult = None

    def __init__(self):
        self.init_config()

    def init_config(self):
        app = Config().get_config('app')
        media = Config().get_config('media')
        # 默认语言
        self._default_language = media.get("tmdb_language", "zh") or "zh"
        # TMDB是否包含成人内容
        self._tmdb_include_adult = media.get("tmdb_include_adult")
        # TMDB
        if app.get('rmt_tmdbkey'):
            # TMDB主体
            self.tmdb = TMDb()
            # 域名
            self.tmdb.domain = Config().get_tmdbapi_url()
            # 开启缓存
            self.tmdb.cache = True
            # APIKEY
            self.tmdb.api_key = app.get('rmt_tmdbkey')
            # 语种
            self.tmdb.language = self._default_language
            # 代理
            self.tmdb.proxies = Config().get_proxies()
            # TMDB是否包含成人内容
            self.tmdb.include_adult = self._tmdb_include_adult
            # 调试模式
            self.tmdb.debug = False
            # 查询对象
            self.search = Search()
            self.movie = Movie()
            self.tv = TV()
            self.episode = Episode()
            self.find = Find()
            self.person = Person()
            self.trending = Trending()
            self.discover = Discover()
            self.genre = Genre()
        # 元数据缓存
        self.meta = MetaHelper()

    def __set_language(self, language):
        """
        设置语言
        :param language: zh/en
        """
        if not self.tmdb:
            return
        if language:
            self.tmdb.language = language
        else:
            self.tmdb.language = self._default_language

    @staticmethod
    def __lookup_parsed(media, raw, mode, language, chinese, append, strict=False):
        query = {"title": media.get_name(), "raw": str(raw), "kind": media.type.name.lower(),
                 "year": str(media.year or ""), "season": int(media.begin_season or 0),
                 "mode": mode, "strict": bool(strict), "language": language or "",
                 "localize": bool(chinese), "append": append}
        try:
            result = _tmdb_query(query, recognize=True)
            if result.get("tv"):
                media.type = MediaType.TV
                if media.begin_season is None:
                    media.set_season(result.get("season"))
                if media.begin_episode is None:
                    media.set_episode(result.get("episode"))
            row = result.get("row")
            if row is None:
                return {}
            info = AsObj(**row)
            info["media_type"] = MediaType.MOVIE if row["media_type"] == "movie" else MediaType.TV
            return info
        except (KeyError, OSError, ValueError, TypeError, subprocess.SubprocessError):
            log.warn("【Meta】媒体名称识别服务暂不可用")
            return None

    def search_tmdb_person(self, name):
        """
        搜索TMDB演员信息
        """
        if not self.search:
            return []
        try:
            return self.__dict_tmdbpersons(self.search.people({"query": name}))
        except Exception as err:
            print(str(err))
            return []

    def get_tmdb_info(self, mtype: MediaType,
                      tmdbid,
                      language=None,
                      append_to_response=None,
                      chinese=True):
        """
        给定TMDB号，查询一条媒体信息
        :param mtype: 类型：电影、电视剧、动漫，为空时都查（此时用不上年份）
        :param tmdbid: TMDB的ID，有tmdbid时优先使用tmdbid，否则使用年份和标题
        :param language: 语种
        :param append_to_response: 附加信息
        :param chinese: 是否转换中文标题
        """
        query = {"kind": "movie" if mtype == MediaType.MOVIE else "tv", "id": str(tmdbid),
                 "language": language or "", "append": append_to_response, "localize": bool(chinese)}
        try:
            info = _tmdb_detail(query)
            info["media_type"] = MediaType.MOVIE if query["kind"] == "movie" else MediaType.TV
            return info
        except (KeyError, OSError, TypeError, ValueError, subprocess.SubprocessError):
            log.warn("【Meta】TMDB 媒体详情暂不可用")
            return None

    def get_tmdb_infos(self, title, year=None, mtype: MediaType = None, language=None, page=1):
        """
        查询名称中有关键字的所有的TMDB信息并返回
        """
        if not self.tmdb:
            log.error("【Meta】TMDB API Key 未设置！")
            return []
        if not title:
            return []
        kind = "movie" if mtype == MediaType.MOVIE else "tv" if mtype else ""
        query = {"title": title, "year": str(year or ""), "kind": kind, "page": int(page)}
        try:
            rows = json.loads(run_go_helper("search-titles", "tmdb", payload=json.dumps(query).encode(), timeout=40))
        except (OSError, ValueError, subprocess.SubprocessError):
            raise TMDbException("TMDB 词条搜索暂不可用") from None
        results = []
        for row in rows:
            info = AsObj(**row["info"])
            info["media_type"] = MediaType.MOVIE if row["kind"] == "movie" else MediaType.TV
            results.append(info)
        return results

    @staticmethod
    def __make_cache_key(meta_info):
        """
        生成缓存的key
        """
        if not meta_info:
            return None
        return f"[{meta_info.type.value}]{meta_info.get_name()}-{meta_info.year}-{meta_info.begin_season}"

    def get_cache_info(self, meta_info):
        """
        根据名称查询是否已经有缓存
        """
        if not meta_info:
            return {}
        return self.meta.get_meta_data_by_key(self.__make_cache_key(meta_info))

    @staticmethod
    def apply_cached_details(media, details, entry, language):
        media.set_tmdb_info(details)
        if details and entry and not language and str(details.get("id")) == str(entry.get("id")):
            if entry.get("title"):
                media.title = entry["title"]
            if entry.get("year"):
                media.year = entry["year"]

    def get_media_info(self, title,
                       subtitle=None,
                       mtype=None,
                       strict=None,
                       cache=True,
                       language=None,
                       chinese=True,
                       append_to_response=None, *, parsed=None, cache_info=None):
        """
        只有名称信息，判别是电影还是电视剧并搜刮TMDB信息，用于种子名称识别
        :param title: 种子名称
        :param subtitle: 种子副标题
        :param mtype: 类型：电影、电视剧、动漫
        :param strict: 是否严格模式，为true时，不会再去掉年份再查一次
        :param cache: 是否使用缓存，默认TRUE
        :param language: 语言
        :param chinese: 原标题为英文时是否从别名中搜索中文名称
        :param append_to_response: 额外查询的信息
        :return: 带有TMDB信息的MetaInfo对象
        """
        if not self.tmdb:
            log.error("【Meta】TMDB API Key 未设置！")
            return None
        if not title:
            return None
        # 设置语言
        self.__set_language(language)
        # 识别
        meta_info = parsed if parsed is not None else MetaInfo(title, subtitle=subtitle)
        if not meta_info.get_name() or not meta_info.type:
            log.warn("【Rmt】%s 未识别出有效信息！" % meta_info.org_string)
            return None
        if mtype:
            meta_info.type = mtype
        media_key = self.__make_cache_key(meta_info)
        if cache and cache_info is None:
            cache_info = self.meta.get_meta_data_by_key(media_key)
        if not cache or not cache_info:
            file_media_info = self.__lookup_parsed(meta_info, title, "title", language, chinese, append_to_response, strict)
            # 保存到缓存
            if file_media_info is not None:
                self.__insert_media_cache(media_key=media_key,
                                          file_media_info=file_media_info)
        else:
            # 使用缓存信息
            if cache_info.get("id"):
                file_media_info = self.get_tmdb_info(mtype=cache_info.get("type"),
                                                     tmdbid=cache_info.get("id"), language=language,
                                                     chinese=chinese,
                                                     append_to_response=append_to_response)
            else:
                file_media_info = None
        # 赋值TMDB信息并返回
        self.apply_cached_details(meta_info, file_media_info, cache_info if cache else None, language)
        return meta_info

    def __insert_media_cache(self, media_key, file_media_info):
        """
        将TMDB信息插入缓存
        """
        if file_media_info:
            # 缓存标题
            cache_title = file_media_info.get(
                "title") if file_media_info.get(
                "media_type") == MediaType.MOVIE else file_media_info.get("name")
            # 缓存年份
            cache_year = file_media_info.get('release_date') if file_media_info.get(
                "media_type") == MediaType.MOVIE else file_media_info.get('first_air_date')
            if cache_year:
                cache_year = cache_year[:4]
            self.meta.update_meta_data({
                media_key: {
                    "id": file_media_info.get("id"),
                    "type": file_media_info.get("media_type"),
                    "year": cache_year,
                    "title": cache_title,
                    "poster_path": file_media_info.get("poster_path"),
                    "backdrop_path": file_media_info.get("backdrop_path")
                }
            })
        else:
            self.meta.update_meta_data({media_key: {'id': 0}})

    def get_media_info_on_files(self,
                                file_list,
                                tmdb_info=None,
                                media_type=None,
                                season=None,
                                episode_format: EpisodeFormat = None,
                                language=None,
                                chinese=True,
                                append_to_response=None):
        """
        根据文件清单，搜刮TMDB信息，用于文件名称的识别
        :param file_list: 文件清单，如果是列表也可以是单个文件，也可以是一个目录
        :param tmdb_info: 如有传入TMDB信息则以该TMDB信息赋于所有文件，否则按名称从TMDB搜索，用于手工识别时传入
        :param media_type: 媒体类型：电影、电视剧、动漫，如有传入以该类型赋于所有文件，否则按名称从TMDB搜索并识别
        :param season: 季号，如有传入以该季号赋于所有文件，否则从名称中识别
        :param episode_format: EpisodeFormat
        :param language: 语言
        :param chinese: 原标题为英文时是否从别名中搜索中文名称
        :param append_to_response: 附加信息
        :return: 带有TMDB信息的每个文件对应的MetaInfo对象字典
        """
        # 存储文件路径与媒体的对应关系
        if not self.tmdb:
            log.error("【Meta】TMDB API Key 未设置！")
            return {}
        # 设置语言
        self.__set_language(language)
        # 返回结果
        return_media_infos = {}
        # 不是list的转为list
        if not isinstance(file_list, list):
            file_list = [file_list]
        # 遍历每个文件，看得出来的名称是不是不一样，不一样的先搜索媒体信息
        for file_path in file_list:
            try:
                if not os.path.exists(file_path):
                    log.warn("【Meta】%s 不存在" % file_path)
                    continue
                # 解析媒体名称
                # 先用自己的名称
                file_name = os.path.basename(file_path)
                parent_name = os.path.basename(os.path.dirname(file_path))
                parent_parent_name = os.path.basename(PathUtils.get_parent_paths(file_path, 2))
                # 过滤掉蓝光原盘目录下的子文件
                if not os.path.isdir(file_path) \
                        and PathUtils.get_bluray_dir(file_path):
                    log.info("【Meta】%s 跳过蓝光原盘文件：" % file_path)
                    continue
                # 没有自带TMDB信息
                if not tmdb_info:
                    # 识别名称
                    meta_info = MetaInfo(title=file_name, filePath=file_path)
                    # 识别不到则使用上级的名称
                    if not meta_info.get_name() or not meta_info.year:
                        parent_info = MetaInfo(parent_name)
                        if not parent_info.get_name() or not parent_info.year:
                            parent_parent_info = MetaInfo(parent_parent_name)
                            parent_info.type = parent_parent_info.type if parent_parent_info.type and parent_info.type != MediaType.TV else parent_info.type
                            parent_info.cn_name = parent_parent_info.cn_name if parent_parent_info.cn_name else parent_info.cn_name
                            parent_info.en_name = parent_parent_info.en_name if parent_parent_info.en_name else parent_info.en_name
                            parent_info.year = parent_parent_info.year if parent_parent_info.year else parent_info.year
                            parent_info.begin_season = NumberUtils.max_ele(parent_info.begin_season,
                                                                           parent_parent_info.begin_season)
                        if not meta_info.get_name():
                            meta_info.cn_name = parent_info.cn_name
                            meta_info.en_name = parent_info.en_name
                        if not meta_info.year:
                            meta_info.year = parent_info.year
                        if parent_info.type and parent_info.type == MediaType.TV \
                                and meta_info.type != MediaType.TV:
                            meta_info.type = parent_info.type
                        if meta_info.type == MediaType.TV:
                            meta_info.begin_season = NumberUtils.max_ele(parent_info.begin_season,
                                                                         meta_info.begin_season)
                    if not meta_info.get_name() or not meta_info.type:
                        log.warn("【Rmt】%s 未识别出有效信息！" % meta_info.org_string)
                        continue
                    # 区配缓存及TMDB
                    media_key = self.__make_cache_key(meta_info)
                    cache_info = self.meta.get_meta_data_by_key(media_key)
                    if not cache_info:
                        file_media_info = self.__lookup_parsed(meta_info, file_path, "file", language, chinese, append_to_response)
                        # 保存到缓存
                        if file_media_info is not None:
                            self.__insert_media_cache(media_key=media_key,
                                                      file_media_info=file_media_info)
                    else:
                        # 使用缓存信息
                        if cache_info.get("id"):
                            file_media_info = self.get_tmdb_info(mtype=cache_info.get("type"),
                                                                 tmdbid=cache_info.get("id"), language=language,
                                                                 chinese=chinese,
                                                                 append_to_response=append_to_response)
                        else:
                            # 缓存为未识别
                            file_media_info = None
                    # 赋值TMDB信息
                    self.apply_cached_details(meta_info, file_media_info, cache_info, language)
                # 自带TMDB信息
                else:
                    meta_info = MetaInfo(title=file_name, mtype=media_type, filePath=file_path)
                    meta_info.set_tmdb_info(tmdb_info)
                    if season and meta_info.type != MediaType.MOVIE:
                        meta_info.begin_season = int(season)
                    if episode_format:
                        begin_ep, end_ep, part = episode_format.split_episode(file_name)
                        if begin_ep is not None:
                            meta_info.begin_episode = begin_ep
                            meta_info.part = part
                        if end_ep is not None:
                            meta_info.end_episode = end_ep
                    # 加入缓存
                    self.save_rename_cache(file_name, tmdb_info)
                # 按文件路程存储
                return_media_infos[file_path] = meta_info
            except Exception as err:
                print(str(err))
                log.error("【Rmt】发生错误：%s - %s" % (str(err), traceback.format_exc()))
        # 循环结束
        return return_media_infos

    def __dict_tmdbpersons(self, infos, chinese=True):
        """
        TMDB人员信息转为字典
        """
        if not infos:
            return []
        ret_infos = []
        for info in infos:
            if chinese:
                name = self.get_tmdbperson_chinese_name(person_id=info.get("id")) or info.get("name")
            else:
                name = info.get("name")
            tmdbid = info.get("id")
            image = Config().get_tmdbimage_url(info.get("profile_path"), prefix="h632") \
                if info.get("profile_path") else ""
            ret_infos.append({
                "id": tmdbid,
                "name": name,
                "role": info.get("name") if info.get("name") != name else "",
                "image": image
            })
        return ret_infos

    @staticmethod
    def __dict_tmdbinfos(infos, mtype=None, poster_filter=False):
        """
        TMDB电影信息转为字典
        """
        if not infos:
            return []
        ret_infos = []
        for info in infos:
            tmdbid = info.get("id")
            vote = round(float(info.get("vote_average")), 1) if info.get("vote_average") else 0,
            image = Config().get_tmdbimage_url(info.get("poster_path"))
            if poster_filter and not image:
                continue
            overview = info.get("overview")
            if mtype:
                media_type = mtype.value
                year = info.get("release_date")[0:4] if info.get(
                    "release_date") and mtype == MediaType.MOVIE else info.get(
                    "first_air_date")[0:4] if info.get(
                    "first_air_date") else ""
                typestr = 'MOV' if mtype == MediaType.MOVIE else 'TV'
                title = info.get("title") if mtype == MediaType.MOVIE else info.get("name")
            else:
                media_type = MediaType.MOVIE.value if info.get(
                    "media_type") == "movie" else MediaType.TV.value
                year = info.get("release_date")[0:4] if info.get(
                    "release_date") and info.get(
                    "media_type") == "movie" else info.get(
                    "first_air_date")[0:4] if info.get(
                    "first_air_date") else ""
                typestr = 'MOV' if info.get("media_type") == "movie" else 'TV'
                title = info.get("title") if info.get("media_type") == "movie" else info.get("name")

            ret_infos.append({
                'id': tmdbid,
                'orgid': tmdbid,
                'tmdbid': tmdbid,
                'title': title,
                'type': typestr,
                'media_type': media_type,
                'year': year,
                'vote': vote,
                'image': image,
                'overview': overview
            })

        return ret_infos

    def get_tmdb_hot_movies(self, page):
        """
        获取热门电影
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.movie:
            return []
        return self.__dict_tmdbinfos(self.movie.popular(page), MediaType.MOVIE)

    def get_tmdb_hot_tvs(self, page):
        """
        获取热门电视剧
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.tv:
            return []
        return self.__dict_tmdbinfos(self.tv.popular(page), MediaType.TV)

    def get_tmdb_new_movies(self, page):
        """
        获取最新电影
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.movie:
            return []
        return self.__dict_tmdbinfos(self.movie.now_playing(page), MediaType.MOVIE)

    def get_tmdb_new_tvs(self, page):
        """
        获取最新电视剧
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.tv:
            return []
        return self.__dict_tmdbinfos(self.tv.on_the_air(page), MediaType.TV)

    def get_tmdb_upcoming_movies(self, page):
        """
        获取即将上映电影
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.movie:
            return []
        return self.__dict_tmdbinfos(self.movie.upcoming(page), MediaType.MOVIE)

    def get_tmdb_trending_all_week(self, page=1):
        """
        获取即将上映电影
        :param page: 第几页
        :return: TMDB信息列表
        """
        if not self.movie:
            return []
        return self.__dict_tmdbinfos(self.trending.all_week(page=page))

    def get_tmdb_tv_season_detail(self, tmdbid, season: int):
        """获取一季的剧集、图片和演职员资料。"""
        try:
            return _tmdb_detail({"kind": "tv", "id": str(tmdbid), "season": int(season)})
        except (KeyError, OSError, ValueError, TypeError, subprocess.SubprocessError):
            log.warn("【Meta】TMDB 季详情暂不可用")
            return {}

    def get_tmdb_tv_seasons_byid(self, tmdbid):
        """
        根据TMDB查询TMDB电视剧的所有季
        """
        if not tmdbid:
            return []
        return self.get_tmdb_tv_seasons(
            tv_info=self.get_tmdb_info(MediaType.TV, tmdbid, chinese=False)
        )

    @staticmethod
    def get_tmdb_tv_seasons(tv_info):
        """
        查询TMDB电视剧的所有季
        :param tv_info: TMDB 的季信息
        :return: 带有season_number、episode_count 的每季总集数的字典列表
        """
        """
        "seasons": [
            {
              "air_date": "2006-01-08",
              "episode_count": 11,
              "id": 3722,
              "name": "特别篇",
              "overview": "",
              "poster_path": "/snQYndfsEr3Sto2jOmkmsQuUXAQ.jpg",
              "season_number": 0
            },
            {
              "air_date": "2005-03-27",
              "episode_count": 9,
              "id": 3718,
              "name": "第 1 季",
              "overview": "",
              "poster_path": "/foM4ImvUXPrD2NvtkHyixq5vhPx.jpg",
              "season_number": 1
            }
        ]
        """
        if not tv_info:
            return []
        ret_info = []
        for info in tv_info.get("seasons") or []:
            if not info.get("season_number"):
                continue
            ret_info.append({
                "air_date": info.get("air_date"),
                "episode_count": info.get("episode_count"),
                "id": info.get("id"),
                "name": info.get("name"),
                "overview": info.get("overview"),
                "poster_path": Config().get_tmdbimage_url(info.get("poster_path")) if info.get("poster_path") else "",
                "season_number": info.get("season_number")
            })
        ret_info.reverse()
        return ret_info

    def get_tmdb_season_episodes(self, tmdbid, season: int):
        """
        :param: tmdbid: TMDB ID
        :param: season: 季号
        """
        """
        从TMDB的季集信息中获得某季的集信息
        """
        """
        "episodes": [
            {
              "air_date": "2023-01-15",
              "episode_number": 1,
              "id": 2181581,
              "name": "当你迷失在黑暗中",
              "overview": "在一场全球性的流行病摧毁了文明之后，一个顽强的幸存者负责照顾一个 14 岁的小女孩，她可能是人类最后的希望。",
              "production_code": "",
              "runtime": 81,
              "season_number": 1,
              "show_id": 100088,
              "still_path": "/aRquEWm8wWF1dfa9uZ1TXLvVrKD.jpg",
              "vote_average": 8,
              "vote_count": 33
            },
          ]
        """
        if not tmdbid:
            return []
        season_info = self.get_tmdb_tv_season_detail(tmdbid=tmdbid, season=season)
        if not season_info:
            return []
        ret_info = []
        for info in season_info.get("episodes") or []:
            ret_info.append({
                "air_date": info.get("air_date"),
                "episode_number": info.get("episode_number"),
                "id": info.get("id"),
                "name": info.get("name"),
                "overview": info.get("overview"),
                "production_code": info.get("production_code"),
                "runtime": info.get("runtime"),
                "season_number": info.get("season_number"),
                "show_id": info.get("show_id"),
                "still_path": Config().get_tmdbimage_url(info.get("still_path")) if info.get("still_path") else "",
                "vote_average": info.get("vote_average")
            })
        ret_info.reverse()
        return ret_info

    def get_tmdb_backdrop(self, mtype, tmdbid):
        """
        获取TMDB的背景图
        """
        if not tmdbid:
            return ""
        tmdbinfo = self.get_tmdb_info(mtype=mtype,
                                      tmdbid=tmdbid,
                                      append_to_response="images",
                                      chinese=False)
        if not tmdbinfo:
            return ""
        results = self.get_tmdb_backdrops(tmdbinfo=tmdbinfo, original=False)
        return results[0] if results else ""

    @staticmethod
    def get_tmdb_backdrops(tmdbinfo, original=True):
        """
        获取TMDB的背景图
        """
        """
        {
          "backdrops": [
            {
              "aspect_ratio": 1.778,
              "height": 2160,
              "iso_639_1": "en",
              "file_path": "/qUroDlCDUMwRWbkyjZGB9THkMgZ.jpg",
              "vote_average": 5.312,
              "vote_count": 1,
              "width": 3840
            },
            {
              "aspect_ratio": 1.778,
              "height": 2160,
              "iso_639_1": "en",
              "file_path": "/iyxvxEQIfQjzJJTfszZxmH5UV35.jpg",
              "vote_average": 0,
              "vote_count": 0,
              "width": 3840
            },
            {
              "aspect_ratio": 1.778,
              "height": 720,
              "iso_639_1": "en",
              "file_path": "/8SRY6IcMKO1E5p83w7bjvcqklp9.jpg",
              "vote_average": 0,
              "vote_count": 0,
              "width": 1280
            },
            {
              "aspect_ratio": 1.778,
              "height": 1080,
              "iso_639_1": "en",
              "file_path": "/erkJ7OxJWFdLBOcn2MvIdhTLHTu.jpg",
              "vote_average": 0,
              "vote_count": 0,
              "width": 1920
            }
          ]
        }
        """
        if not tmdbinfo:
            return []
        prefix_url = Config().get_tmdbimage_url(r"%s", prefix="original") \
            if original else Config().get_tmdbimage_url(r"%s")
        backdrops = tmdbinfo.get("images", {}).get("backdrops") or []
        result = [prefix_url % backdrop.get("file_path") for backdrop in backdrops]
        result.append(prefix_url % tmdbinfo.get("backdrop_path"))
        return result

    @staticmethod
    def get_tmdb_season_episodes_num(tv_info, season: int):
        """
        从TMDB的季信息中获得具体季有多少集
        :param season: 季号，数字
        :param tv_info: 已获取的TMDB季的信息
        :return: 该季的总集数
        """
        if not tv_info:
            return 0
        seasons = tv_info.get("seasons")
        if not seasons:
            return 0
        for sea in seasons:
            if sea.get("season_number") == int(season):
                return int(sea.get("episode_count"))
        return 0

    @staticmethod
    def __dict_media_crews(crews):
        """
        字典化媒体工作人员
        """
        return [{
            "id": crew.get("id"),
            "gender": crew.get("gender"),
            "known_for_department": crew.get("known_for_department"),
            "name": crew.get("name"),
            "original_name": crew.get("original_name"),
            "popularity": crew.get("popularity"),
            "image": Config().get_tmdbimage_url(crew.get("profile_path"), prefix="h632"),
            "credit_id": crew.get("credit_id"),
            "department": crew.get("department"),
            "job": crew.get("job"),
            "profile": 'https://www.themoviedb.org/person/%s' % crew.get('id')
        } for crew in crews or []]

    @staticmethod
    def __dict_media_casts(casts):
        """
        字典化媒体演职人员
        """
        return [{
            "id": cast.get("id"),
            "gender": cast.get("gender"),
            "known_for_department": cast.get("known_for_department"),
            "name": cast.get("name"),
            "original_name": cast.get("original_name"),
            "popularity": cast.get("popularity"),
            "image": Config().get_tmdbimage_url(cast.get("profile_path"), prefix="h632"),
            "cast_id": cast.get("cast_id"),
            "role": cast.get("character"),
            "credit_id": cast.get("credit_id"),
            "order": cast.get("order"),
            "profile": 'https://www.themoviedb.org/person/%s' % cast.get('id')
        } for cast in casts or []]

    def get_tmdb_directors_actors(self, tmdbinfo):
        """
        查询导演和演员
        :param tmdbinfo: TMDB元数据
        :return: 导演列表，演员列表
        """
        """
        "cast": [
          {
            "adult": false,
            "gender": 2,
            "id": 3131,
            "known_for_department": "Acting",
            "name": "Antonio Banderas",
            "original_name": "Antonio Banderas",
            "popularity": 60.896,
            "profile_path": "/iWIUEwgn2KW50MssR7tdPeFoRGW.jpg",
            "cast_id": 2,
            "character": "Puss in Boots (voice)",
            "credit_id": "6052480e197de4006bb47b9a",
            "order": 0
          }
        ],
        "crew": [
          {
            "adult": false,
            "gender": 2,
            "id": 5524,
            "known_for_department": "Production",
            "name": "Andrew Adamson",
            "original_name": "Andrew Adamson",
            "popularity": 9.322,
            "profile_path": "/qqIAVKAe5LHRbPyZUlptsqlo4Kb.jpg",
            "credit_id": "63b86b2224b33300a0585bf1",
            "department": "Production",
            "job": "Executive Producer"
          }
        ]
        """
        if not tmdbinfo:
            return [], []
        _credits = tmdbinfo.get("credits")
        if not _credits:
            return [], []
        directors = []
        actors = []
        for cast in self.__dict_media_casts(_credits.get("cast")):
            if cast.get("known_for_department") == "Acting":
                actors.append(cast)
        for crew in self.__dict_media_crews(_credits.get("crew")):
            if crew.get("job") == "Director":
                directors.append(crew)
        return directors, actors

    def get_tmdb_cats(self, mtype, tmdbid):
        """
        获取TMDB的演员列表
        :param: mtype: 媒体类型
        :param: tmdbid: TMDBID
        """
        try:
            if mtype == MediaType.MOVIE:
                if not self.movie:
                    return []
                return self.__dict_media_casts(self.movie.credits(tmdbid).get("cast"))
            else:
                if not self.tv:
                    return []
                return self.__dict_media_casts(self.tv.credits(tmdbid).get("cast"))
        except Exception as err:
            print(str(err))
        return []

    @staticmethod
    def get_tmdb_genres_names(tmdbinfo):
        """
        从TMDB数据中获取风格名称
        """
        """
        "genres": [
            {
              "id": 16,
              "name": "动画"
            },
            {
              "id": 28,
              "name": "动作"
            },
            {
              "id": 12,
              "name": "冒险"
            },
            {
              "id": 35,
              "name": "喜剧"
            },
            {
              "id": 10751,
              "name": "家庭"
            },
            {
              "id": 14,
              "name": "奇幻"
            }
          ]
        """
        if not tmdbinfo:
            return ""
        genres = tmdbinfo.get("genres") or []
        genres_list = [genre.get("name") for genre in genres]
        return ", ".join(genres_list) if genres_list else ""

    def get_tmdb_genres(self, mtype):
        """
        获取TMDB的风格列表
        :param: mtype: 媒体类型
        """
        if not self.genre:
            return []
        try:
            if mtype == MediaType.MOVIE:
                return self.genre.movie_list()
            else:
                return self.genre.tv_list()
        except Exception as err:
            print(str(err))
        return []

    @staticmethod
    def get_get_production_country_names(tmdbinfo):
        """
        从TMDB数据中获取制片国家名称
        """
        """
        "production_countries": [
            {
              "iso_3166_1": "US",
              "name": "美国"
            }
          ]
        """
        if not tmdbinfo:
            return ""
        countries = tmdbinfo.get("production_countries") or []
        countries_list = [country.get("name") for country in countries]
        return ", ".join(countries_list) if countries_list else ""

    @staticmethod
    def get_tmdb_production_company_names(tmdbinfo):
        """
        从TMDB数据中获取制片公司名称
        """
        """
        "production_companies": [
            {
              "id": 2,
              "logo_path": "/wdrCwmRnLFJhEoH8GSfymY85KHT.png",
              "name": "DreamWorks Animation",
              "origin_country": "US"
            }
          ]
        """
        if not tmdbinfo:
            return ""
        companies = tmdbinfo.get("production_companies") or []
        companies_list = [company.get("name") for company in companies]
        return ", ".join(companies_list) if companies_list else ""

    @staticmethod
    def get_tmdb_crews(tmdbinfo, nums=None):
        """
        从TMDB数据中获取制片人员
        """
        if not tmdbinfo:
            return ""
        crews = tmdbinfo.get("credits", {}).get("crew") or []
        result = [{crew.get("name"): crew.get("job")} for crew in crews]
        if nums:
            return result[:nums]
        else:
            return result

    def get_tmdb_en_title(self, media_info):
        """
        获取TMDB的英文名称
        """
        en_info = self.get_tmdb_info(mtype=media_info.type,
                                     tmdbid=media_info.tmdb_id,
                                     language="en",
                                     chinese=False)
        if en_info:
            return en_info.get("title") if media_info.type == MediaType.MOVIE else en_info.get("name")
        return None

    def get_episode_title(self, media_info, language=None):
        """
        获取剧集的标题
        """
        if media_info.type == MediaType.MOVIE:
            return None
        # 设置语言
        self.__set_language(language)
        if media_info.tmdb_id:
            if not media_info.begin_episode:
                return None
            episodes = self.get_tmdb_season_episodes(tmdbid=media_info.tmdb_id,
                                                     season=int(media_info.get_season_seq()))
            for episode in episodes:
                if episode.get("episode_number") == media_info.begin_episode:
                    return episode.get("name")
        return None

    def get_movie_similar(self, tmdbid, page=1):
        """
        查询类似电影
        """
        if not self.movie:
            return []
        try:
            movies = self.movie.similar(movie_id=tmdbid, page=page) or []
            return self.__dict_tmdbinfos(movies, MediaType.MOVIE)
        except Exception as e:
            print(str(e))
            return []

    def get_movie_recommendations(self, tmdbid, page=1):
        """
        查询电影关联推荐
        """
        if not self.movie:
            return []
        try:
            movies = self.movie.recommendations(movie_id=tmdbid, page=page) or []
            return self.__dict_tmdbinfos(movies, MediaType.MOVIE)
        except Exception as e:
            print(str(e))
            return []

    def get_tv_similar(self, tmdbid, page=1):
        """
        查询类似电视剧
        """
        if not self.tv:
            return []
        try:
            tvs = self.tv.similar(tv_id=tmdbid, page=page) or []
            return self.__dict_tmdbinfos(tvs, MediaType.TV)
        except Exception as e:
            print(str(e))
            return []

    def get_tv_recommendations(self, tmdbid, page=1):
        """
        查询电视剧关联推荐
        """
        if not self.tv:
            return []
        try:
            tvs = self.tv.recommendations(tv_id=tmdbid, page=page) or []
            return self.__dict_tmdbinfos(tvs, MediaType.TV)
        except Exception as e:
            print(str(e))
            return []

    def get_tmdb_discover(self, mtype, params=None, page=1):
        """
        浏览电影、电视剧（复杂过滤条件）
        """
        if not self.discover:
            return []
        try:
            if mtype == MediaType.MOVIE:
                movies = self.discover.discover_movies(params=params, page=page)
                return self.__dict_tmdbinfos(infos=movies, mtype=mtype, poster_filter=True)
            elif mtype == MediaType.TV:
                tvs = self.discover.discover_tv_shows(params=params, page=page)
                return self.__dict_tmdbinfos(infos=tvs, mtype=mtype, poster_filter=True)
        except Exception as e:
            print(str(e))
        return []

    def get_tmdb_discover_movies_pages(self, params=None):
        """
        获取电影浏览的总页数
        """
        if not self.discover:
            return 0
        try:
            return self.discover.discover_movies_pages(params=params)
        except Exception as e:
            print(str(e))
        return 0

    def get_person_medias(self, personid, mtype=None, page=1):
        """
        查询人物相关影视作品
        """
        if not self.person:
            return []
        try:
            if mtype == MediaType.MOVIE:
                movies = self.person.movie_credits(person_id=personid) or []
                result = self.__dict_tmdbinfos(movies, mtype)
            elif mtype:
                tvs = self.person.tv_credits(person_id=personid) or []
                result = self.__dict_tmdbinfos(tvs, mtype)
            else:
                medias = self.person.combined_credits(person_id=personid) or []
                result = self.__dict_tmdbinfos(medias)
            return result[(page - 1) * 20: page * 20]
        except Exception as e:
            print(str(e))
        return []

    def get_tmdb_us_title(self, tmdbinfo):
        """
        从别名中获取中文标题
        """
        if not tmdbinfo:
            return None
        if tmdbinfo.get("media_type") == MediaType.MOVIE:
            alternative_titles = tmdbinfo.get("alternative_titles", {}).get("titles", [])
        else:
            alternative_titles = tmdbinfo.get("alternative_titles", {}).get("results", [])
        for alternative_title in alternative_titles:
            iso_3166_1 = alternative_title.get("iso_3166_1")
            if iso_3166_1 == "US":
                title = alternative_title.get("title")
                if title:
                    return title
        return tmdbinfo.get("title") if tmdbinfo.get("media_type") == MediaType.MOVIE else tmdbinfo.get("name")

    def get_tmdbperson_chinese_name(self, person_id=None, person_info=None):
        """
        查询TMDB人物中文名称
        """
        if not self.person:
            return ""
        if not person_info and not person_id:
            return ""
        # 返回中文名
        name = ""
        # 所有别名
        alter_names = []
        try:
            if not person_info:
                person_info = self.person.details(person_id)
            if person_info:
                aka_names = person_info.get("also_known_as", []) or []
            else:
                return ""
        except Exception as err:
            print(str(err))
            return ""
        for aka_name in aka_names:
            if StringUtils.is_chinese(aka_name):
                alter_names.append(aka_name)
        if len(alter_names) == 1:
            name = alter_names[0]
        elif len(alter_names) > 1:
            for alter_name in alter_names:
                if alter_name == zhconv.convert(alter_name, 'zh-hans'):
                    name = alter_name
        return name

    def get_tmdbperson_aka_names(self, person_id):
        """
        查询人物又名
        """
        if not self.person:
            return []
        try:
            aka_names = self.person.details(person_id).get("also_known_as", []) or []
            return aka_names
        except Exception as err:
            print(str(err))
            return []

    def get_random_discover_backdrop(self):
        """
        获取TMDB热门电影随机一张背景图
        """
        if not self.discover:
            return ""
        try:
            medias = self.discover.discover_movies(params={"sort_by": "popularity.desc"})
            if medias:
                # 随机一个电影
                media = random.choice(medias)
                img_url = Config().get_tmdbimage_url(media.get("backdrop_path"), prefix="original") \
                    if media.get("backdrop_path") else ''
                img_title = media.get('title', '')
                img_link = f"https://www.themoviedb.org/movie/{media.get('id')}" if media.get('id') else ''
                return img_url, img_title, img_link
        except Exception as err:
            print(str(err))
        return '', '', ''

    def save_rename_cache(self, file_name, cache_info):
        """
        将手动识别的信息加入缓存
        """
        if not file_name or not cache_info:
            return
        meta_info = MetaInfo(title=file_name)
        self.__insert_media_cache(self.__make_cache_key(meta_info), cache_info)

    @staticmethod
    def merge_media_info(target, source):
        """
        将soruce中有效的信息合并到target中并返回
        """
        target.set_tmdb_info(source.tmdb_info)
        target.fanart_poster = source.get_poster_image()
        target.fanart_backdrop = source.get_backdrop_image()
        target.set_download_info(download_setting=source.download_setting,
                                 save_path=source.save_path)
        return target

    def get_tmdbid_by_imdbid(self, imdbid):
        """
        根据IMDBID查询TMDB信息
        """
        if not self.find:
            return None
        try:
            result = self.find.find_by_imdbid(imdbid) or {}
            tmdbinfo = result.get('movie_results') or result.get("tv_results")
            if tmdbinfo:
                tmdbinfo = tmdbinfo[0]
                return tmdbinfo.get("id")
        except Exception as err:
            print(str(err))
        return None

    @staticmethod
    def get_detail_url(mtype, tmdbid):
        """
        获取TMDB/豆瓣详情页地址
        """
        if not tmdbid:
            return ""
        if str(tmdbid).startswith("DB:"):
            return "https://movie.douban.com/subject/%s" % str(tmdbid).replace("DB:", "")
        elif mtype == MediaType.MOVIE:
            return "https://www.themoviedb.org/movie/%s" % tmdbid
        else:
            return "https://www.themoviedb.org/tv/%s" % tmdbid

    def get_episode_images(self, tv_id, season_id, episode_id, orginal=False):
        """
        获取剧集中某一集封面
        """
        if not self.episode:
            return ""
        if not tv_id or not season_id or not episode_id:
            return ""
        res = self.episode.images(tv_id, season_id, episode_id)
        if res:
            if orginal:
                return Config().get_tmdbimage_url(res[-1].get("file_path"), prefix="original")
            else:
                return Config().get_tmdbimage_url(res[-1].get("file_path"))
        else:
            return ""

    def get_tmdb_factinfo(self, media_info):
        """
        获取TMDB发布信息
        """
        result = []
        if media_info.vote_average:
            result.append({"评分": media_info.vote_average})
        if media_info.original_title:
            result.append({"原始标题": media_info.original_title})
        status = media_info.tmdb_info.get("status")
        if status:
            result.append({"状态": status})
        if media_info.release_date:
            result.append({"上映日期": media_info.release_date})
        revenue = media_info.tmdb_info.get("revenue")
        if revenue:
            result.append({"收入": StringUtils.str_amount(revenue)})
        budget = media_info.tmdb_info.get("budget")
        if budget:
            result.append({"成本": StringUtils.str_amount(budget)})
        if media_info.original_language:
            result.append({"原始语言": media_info.original_language})
        production_country = self.get_get_production_country_names(tmdbinfo=media_info.tmdb_info)
        if media_info.networks:
            result.append({"电视网": media_info.networks})
        if production_country:
            result.append({"出品国家": production_country}),
        production_company = self.get_tmdb_production_company_names(tmdbinfo=media_info.tmdb_info)
        if production_company:
            result.append({"制作公司": production_company})

        return result
