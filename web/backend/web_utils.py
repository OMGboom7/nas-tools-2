from functools import lru_cache

import cn2an

from app.media import Media, Bangumi
from app.media.douban import get_douban_detail, search_douban_medias
from app.media.meta import MetaInfo
from app.utils import StringUtils, ExceptionUtils, SystemUtils, RequestUtils, IpUtils
from app.utils.types import MediaType
from config import Config
from version import APP_VERSION


class WebUtils:

    @staticmethod
    def get_location(ip):
        """
        根据IP址查询真实地址
        """
        if not IpUtils.is_ipv4(ip):
            return ""
        url = 'https://sp0.baidu.com/8aQDcjqpAAV3otqbppnN2DJv/api.php?co=&resource_id=6006&t=1529895387942&ie=utf8' \
              '&oe=gbk&cb=op_aladdin_callback&format=json&tn=baidu&' \
              'cb=jQuery110203920624944751099_1529894588086&_=1529894588088&query=%s' % ip
        try:
            r = RequestUtils().get_res(url)
            if r:
                r.encoding = 'gbk'
                html = r.text
                c1 = html.split('location":"')[1]
                c2 = c1.split('","')[0]
                return c2
            else:
                return ""
        except Exception as err:
            ExceptionUtils.exception_traceback(err)
            return ""

    @staticmethod
    def get_current_version():
        """
        获取当前版本号
        """
        commit_id = SystemUtils.execute('git rev-parse HEAD')
        if commit_id and len(commit_id) > 7:
            commit_id = commit_id[:7]
        return "%s %s" % (APP_VERSION, commit_id)

    @staticmethod
    def get_latest_version():
        """
        获取最新版本号
        """
        try:
            releases_update_only = True
            version_res = RequestUtils(proxies=Config().get_proxies()).get_res(
                "https://api.github.com/repos/0xforee/nas-tools/releases/latest")
            commit_res = RequestUtils(proxies=Config().get_proxies()).get_res(
                "https://api.github.com/repos/0xforee/nas-tools/commits/master")
            if version_res and commit_res:
                ver_json = version_res.json()
                commit_json = commit_res.json()
                if releases_update_only:
                    version = f"{ver_json['tag_name']}"
                else:
                    version = f"{ver_json['tag_name']} {commit_json['sha'][:7]}"
                url = ver_json["html_url"]
                return version, url
        except Exception as e:
            ExceptionUtils.exception_traceback(e)
        return None, None

    @staticmethod
    def get_mediainfo_from_id(mtype, mediaid, wait=False):
        """
        根据TMDB/豆瓣/BANGUMI获取媒体信息
        """
        if not mediaid:
            return None
        selector = str(mediaid)
        if selector.startswith("DB:"):
            info = get_douban_detail(selector[3:], mtype=mtype, wait=wait)
            if not info:
                return None
            titles = (info.get("original_title"), info.get("title"))
            year = info.get("year") or ""
            if not mtype:
                mtype = MediaType.TV if info.get("episodes_count") or info.get("type") == "tv" else MediaType.MOVIE
        elif selector.startswith("BG:"):
            info = Bangumi().detail(bid=selector[3:])
            if not info:
                return None
            titles = (info.get("name"), info.get("name_cn"))
            year = (info.get("date") or "")[:4]
            mtype = MediaType.TV
        else:
            info = Media().get_tmdb_info(tmdbid=mediaid, mtype=mtype, append_to_response="all")
            if not info:
                return None
            media = MetaInfo(title=info.get("title") if mtype == MediaType.MOVIE else info.get("name"))
            media.set_tmdb_info(info)
            return media

        resolver = Media()
        for title in dict.fromkeys(titles):
            if title:
                media = resolver.get_media_info(title=f"{title} {year}".strip(), mtype=mtype, append_to_response="all")
                if media and media.tmdb_info:
                    if selector.startswith("DB:"):
                        media.douban_id = selector[3:]
                    return media
        return None

    @staticmethod
    def search_media_infos(keyword, source=None, page=1):
        """
        搜索TMDB或豆瓣词条
        :param: keyword 关键字
        :param: source 渠道 tmdb/douban
        :param: season 季号
        :param: episode 集号
        """
        if not keyword:
            return []
        mtype, key_word, season_num, episode_num, _, content = StringUtils.get_keyword_from_string(keyword)
        if source == "tmdb":
            use_douban_titles = False
        elif source == "douban":
            use_douban_titles = True
        else:
            use_douban_titles = Config().get_config("laboratory").get("use_douban_titles")
        if use_douban_titles:
            medias = search_douban_medias(keyword=key_word, mtype=mtype, season=season_num,
                                          episode=episode_num, page=page)
        else:
            meta_info = MetaInfo(title=content)
            tmdbinfos = Media().get_tmdb_infos(title=meta_info.get_name(),
                                               year=meta_info.year,
                                               mtype=mtype,
                                               page=page)
            medias = []
            for tmdbinfo in tmdbinfos:
                tmp_info = MetaInfo(title=keyword)
                tmp_info.set_tmdb_info(tmdbinfo)
                if meta_info.type != MediaType.MOVIE and tmp_info.type == MediaType.MOVIE:
                    continue
                if tmp_info.begin_season:
                    tmp_info.title = "%s 第%s季" % (tmp_info.title, cn2an.an2cn(meta_info.begin_season, mode='low'))
                if tmp_info.begin_episode:
                    tmp_info.title = "%s 第%s集" % (tmp_info.title, meta_info.begin_episode)
                medias.append(tmp_info)
        return medias

    @staticmethod
    def get_page_range(current_page, total_page):
        """
        计算分页范围
        """
        if total_page <= 5:
            StartPage = 1
            EndPage = total_page
        else:
            if current_page <= 3:
                StartPage = 1
                EndPage = 5
            elif current_page >= total_page - 2:
                StartPage = total_page - 4
                EndPage = total_page
            else:
                StartPage = current_page - 2
                if total_page > current_page + 2:
                    EndPage = current_page + 2
                else:
                    EndPage = total_page
        return range(StartPage, EndPage + 1)

    @staticmethod
    @lru_cache(maxsize=128)
    def request_cache(url):
        """
        带缓存的请求
        """
        if url.find('douban'):
            ret = RequestUtils(referer="https://movie.douban.com").get_res(url)
        else:
            ret = RequestUtils().get_res(url)
        if ret:
            return ret.content
        
        # 避免 lru 缓存失败的情况，exception 不会被缓存
        raise Exception('request failed')
