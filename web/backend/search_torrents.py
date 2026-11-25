import json
import os.path
import subprocess

import log
from app.filter import ResourceFilterUnavailable
from app.downloader import Downloader
from app.helper.db_helper import SearchResultsUnavailable
from app.helper import ProgressHelper
from app.helper.openai_helper import OpenAiHelper
from app.indexer import Indexer
from app.media import Media, DouBan
from app.media.meta import MetaInfo
from app.media.tmdbv3api import TMDbException
from app.media.tmdbv3api.as_obj import AsObj
from app.message import Message
from app.searcher import Searcher
from app.sites import Sites
from app.subscribe import Subscribe
from app.utils import StringUtils, Torrent
from app.utils.types import SearchType, ProgressKey, RssType, MediaType
from config import Config, run_go_helper
from web.backend.web_utils import WebUtils

_choice_fields = ("title", "year", "tmdb_id", "douban_id", "overview", "poster_path", "vote_average",
                  "begin_season", "end_season", "begin_episode", "end_episode",
                  "rss_sites", "search_sites", "download_setting")


def _choice_json(value):
    if isinstance(value, MediaType):
        return value.value
    if isinstance(value, AsObj):
        return dict(value.items())
    raise TypeError("invalid media choice")


def _choice_call(operation, **payload):
    data = json.dumps(payload, default=_choice_json, ensure_ascii=False).encode()
    return json.loads(run_go_helper("search-titles", operation, payload=data))


def _choice_snapshot(media):
    candidate = {field: getattr(media, field) for field in _choice_fields}
    candidate.update(kind=media.type.value, original=media.org_string, tmdb=media.tmdb_info)
    return candidate


def _selected_media(candidate):
    media = MetaInfo(title=candidate["original"])
    if candidate["tmdb"]:
        info = AsObj(**candidate["tmdb"])
        if info.get("media_type"):
            info["media_type"] = MediaType(info["media_type"])
        media.set_tmdb_info(info)
    for field in _choice_fields:
        setattr(media, field, candidate[field])
    media.type = MediaType(candidate["kind"])
    return media


def start_media_message(message, source, user_id, scope):
    if user_id is None or str(user_id) == "":
        return None
    message = str(message).strip()
    if message.isdecimal():
        # Accept decimal numerals from localized keyboards, including full-width digits.
        try:
            number = int(message)
            if number < 10:
                message = str(number)
        except ValueError:
            pass
    conversation = {"channel": f"{source.name}/{scope}", "sender": str(user_id)}
    try:
        decision = _choice_call("start", **conversation, message=message,
                                chatEnabled=OpenAiHelper().get_state())
    except (OSError, ValueError, subprocess.SubprocessError):
        Message().send_channel_msg(channel=source, title="选片服务暂不可用", user_id=user_id, expected_scope=str(scope))
        return None
    return conversation, decision


def search_medias_for_web(content, ident_flag=True, filters=None, tmdbid=None, media_type=None):
    """
    WEB资源搜索
    :param content: 关键字文本，可以包括 类型、标题、季、集、年份等信息，使用 空格分隔，也支持种子的命名格式
    :param ident_flag: 是否进行媒体信息识别
    :param filters: 其它过滤条件
    :param tmdbid: TMDBID或DB:豆瓣ID
    :param media_type: 媒体类型，配合tmdbid传入
    :return: 错误码，错误原因，成功时直接插入数据库
    """
    mtype, key_word, season_num, episode_num, year, content = StringUtils.get_keyword_from_string(content)
    if not key_word:
        log.info("【Web】%s 搜索关键字有误！" % content)
        return -1, "%s 未识别到搜索关键字！" % content
    # 类型
    if media_type:
        mtype = media_type
    # 开始进度
    _searcher = Searcher()
    _process = ProgressHelper()
    _media = Media()
    _process.start(ProgressKey.Search)
    # 识别媒体
    media_info = None
    if ident_flag:

        # 有TMDBID或豆瓣ID
        if tmdbid:
            media_info = WebUtils.get_mediainfo_from_id(mtype=mtype, mediaid=tmdbid)
        else:
            # 按输入名称查
            media_info = _media.get_media_info(mtype=media_type or mtype,
                                               title=content)

        # 整合集
        if media_info:
            if season_num:
                media_info.begin_season = int(season_num)
            if episode_num:
                media_info.begin_episode = int(episode_num)

        if media_info and media_info.tmdb_info:
            # 查询到TMDB信息
            log.info(f"【Web】从TMDB中匹配到{media_info.type.value}：{media_info.get_title_string()}")
            # 查找的季
            if media_info.begin_season is None:
                search_season = None
            else:
                search_season = media_info.get_season_list()
            # 查找的集
            search_episode = media_info.get_episode_list()
            if search_episode and not search_season:
                search_season = [1]
            # 中文名
            if media_info.cn_name:
                search_cn_name = media_info.cn_name
            else:
                search_cn_name = media_info.title
            # 英文名
            search_en_name = None
            if media_info.en_name:
                search_en_name = media_info.en_name
            else:
                if media_info.original_language == "en":
                    search_en_name = media_info.original_title
                else:
                    en_title = _media.get_tmdb_en_title(media_info)
                    if en_title:
                        search_en_name = en_title
            # 两次搜索名称
            second_search_name = None
            if Config().get_config("laboratory").get("search_en_title"):
                if search_en_name:
                    first_search_name = search_en_name
                    second_search_name = search_cn_name
                else:
                    first_search_name = search_cn_name
            else:
                first_search_name = search_cn_name
                if search_en_name:
                    second_search_name = search_en_name

            filter_args = {"season": search_season,
                           "episode": search_episode,
                           "year": media_info.year,
                           "type": media_info.type}
        else:
            # 查询不到数据，使用快速搜索
            log.info(f"【Web】{content} 未从TMDB匹配到媒体信息，将使用快速搜索...")
            ident_flag = False
            media_info = None
            first_search_name = key_word
            second_search_name = None
            filter_args = {
                "season": season_num,
                "episode": episode_num,
                "year": year
            }
    # 快速搜索
    else:
        first_search_name = key_word
        second_search_name = None
        filter_args = {
            "season": season_num,
            "episode": episode_num,
            "year": year
        }
    # 整合高级查询条件
    if filters:
        filter_args.update(filters)
    # 开始搜索
    log.info("【Web】开始搜索 %s ..." % content)
    try:
        media_list = _searcher.search_medias(key_word=first_search_name,
                                             filter_args=filter_args,
                                             match_media=media_info,
                                             in_from=SearchType.WEB)
        # 使用第二名称重新搜索
        if ident_flag \
                and len(media_list) == 0 \
                and second_search_name \
                and second_search_name != first_search_name:
            _process.start(ProgressKey.Search)
            _process.update(ptype=ProgressKey.Search,
                            text="%s 未搜索到资源,尝试通过 %s 重新搜索 ..." % (
                                first_search_name, second_search_name))
            log.info("【Searcher】%s 未搜索到资源,尝试通过 %s 重新搜索 ..." % (first_search_name, second_search_name))
            media_list = _searcher.search_medias(key_word=second_search_name,
                                                 filter_args=filter_args,
                                                 match_media=media_info,
                                                 in_from=SearchType.WEB)
    except ResourceFilterUnavailable as err:
        _process.end(ProgressKey.Search)
        return -1, str(err)
    media_list.sort(key=lambda x: "%s%s%s" % (str(x.res_order).rjust(3, '0'),
                                               str(x.site_order).rjust(3, '0'),
                                               str(x.seeders).rjust(10, '0')), reverse=True)
    try:
        _searcher.replace_search_results(media_list, title=content, ident_flag=ident_flag)
    except SearchResultsUnavailable as err:
        return -1, str(err)
    finally:
        _process.end(ProgressKey.Search)
    if not media_list:
        return 1, "%s 未搜索到任何资源" % content
    log.info(f"【Web】共搜索到 {len(media_list)} 个有效资源")
    return 0, ""


def search_media_by_message(started, in_from: SearchType, user_id, user_name=None):
    conversation, decision = started
    scope = conversation["channel"].partition("/")[2]
    action = decision["action"]
    if action == "ignore":
        return
    if action == "invalid":
        Message().send_channel_msg(expected_scope=scope, channel=in_from, title="输入有误！", user_id=user_id)
        return
    if decision.get("selected"):
        choice = dict(conversation, revision=decision["revision"], index=decision["index"])
        __use_media_choice(action, _selected_media(decision["selected"]), False, in_from, user_id, user_name, scope, choice)
        return
    input_str = decision.get("query", "")
    if not input_str:
        Message().send_channel_msg(expected_scope=scope, channel=in_from, title="无法识别搜索内容！", user_id=user_id)
        return
    # 下载链接
    if action == "download":
        # 检查是不是有这个站点
        site_info = Sites().get_sites(siteurl=input_str)
        # 偿试下载种子文件
        filepath, content, retmsg = Torrent().save_torrent_file(
            url=input_str,
            cookie=site_info.get("cookie"),
            ua=site_info.get("ua"),
            proxy=site_info.get("proxy")
        )
        # 下载种子出错
        if (not content or not filepath) and retmsg:
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title=retmsg,
                                       user_id=user_id)
            return
        # 识别文件名
        filename = os.path.basename(filepath)
        # 识别
        meta_info = Media().get_media_info(title=filename)
        if not meta_info:
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title="无法识别种子文件名！",
                                       user_id=user_id)
            return
        # 开始下载
        meta_info.set_torrent_info(enclosure=input_str)
        Downloader().download(media_info=meta_info,
                              torrent_file=filepath,
                              in_from=in_from,
                              user_name=user_name)
    # 聊天
    elif action == "ask":
        # 调用ChatGPT Api
        answer = OpenAiHelper().get_answer(text=input_str,
                                           userid=user_id)
        if not answer:
            answer = "ChatGTP出错了，请检查OpenAI API Key是否正确，如需搜索电影/电视剧，请发送 搜索或下载 + 名称"
        # 发送消息
        Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                   title="",
                                   text=str(answer).strip(),
                                   user_id=user_id)
    # 搜索或订阅
    else:
        try:
            criteria = _choice_call("criteria", title=input_str,
                                    rss=[site.get("name") for site in Sites().get_sites(rss=True)],
                                    indexers=[indexer.name for indexer in Indexer().get_indexers()],
                                    settings=[{"id": dl.get("id"), "name": dl.get("name")}
                                              for dl in Downloader().get_download_setting().values()])
        except (OSError, ValueError, subprocess.SubprocessError):
            Message().send_channel_msg(expected_scope=scope, channel=in_from, title="选片条件暂不可用", user_id=user_id)
            return
        content = criteria["title"]
        # 识别媒体信息，列出匹配到的所有媒体
        log.info("【Web】正在识别 %s 的媒体信息..." % content)
        if not content:
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title="无法识别搜索内容！",
                                       user_id=user_id)
            return

        # 搜索名称
        try:
            medias = WebUtils.search_media_infos(keyword=content)
        except TMDbException:
            Message().send_channel_msg(expected_scope=scope, channel=in_from, title="TMDB 词条搜索暂不可用", user_id=user_id)
            return
        if not medias:
            # 查询不到媒体信息
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title="%s 查询不到媒体信息！" % content,
                                       user_id=user_id)
            return

        choices = medias[:8]
        for media in choices:
            media.rss_sites = criteria["rss"]
            media.search_sites = criteria["indexers"]
            media.set_download_info(download_setting=criteria["setting"])
        try:
            result = _choice_call("choices", **conversation, revision=decision["revision"],
                                  choices=[_choice_snapshot(media) for media in choices])
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            Message().send_channel_msg(expected_scope=scope, channel=in_from, title="选片结果暂不可用", user_id=user_id)
            return
        if result["action"] == "menu":
            Message().send_channel_list_msg(expected_scope=scope, channel=in_from, title=f"共找到{len(choices)}条相关信息，请回复对应序号",
                                            medias=choices, user_id=user_id)
        elif result.get("selected"):
            choice = dict(conversation, revision=result["revision"], index=result["index"])
            __use_media_choice(result["action"], choices[0], True, in_from, user_id, user_name, scope, choice)


def __use_media_choice(action, media, single, channel, user_id, user_name, scope, choice):
    choice["selector"] = f"DB:{media.douban_id}" if media.douban_id else str(media.tmdb_id)
    if scope and not Message().get_interactive_client(channel, scope):
        return
    if action == "subscribe":
        __rss_media(channel, media, user_id, user_name=user_name, scope=scope, choice=choice)
        return
    if media.douban_id:
        original = media
        details = DouBan().get_media_detail_from_web(media.douban_id) if not single else None
        if details and details.get("imdbid"):
            tmdb_id = Media().get_tmdbid_by_imdbid(details["imdbid"])
            if tmdb_id:
                media.set_tmdb_info(Media().get_tmdb_info(mtype=media.type, tmdbid=tmdb_id))
                media.imdb_id = details["imdbid"]
        else:
            media = Media().get_media_info(title=f"{media.title} {media.year}", mtype=media.type, strict=True)
        if not media or not media.tmdb_info:
            Message().send_channel_msg(expected_scope=scope, channel=channel, title=f"{original.get_title_string()} 从TMDB查询不到媒体信息！",
                                       user_id=user_id)
            return
        media.rss_sites, media.search_sites = original.rss_sites, original.search_sites
        media.set_download_info(download_setting=original.download_setting)
        if original.begin_episode is not None:
            media.begin_episode = original.begin_episode
        if original.begin_season is not None:
            media.begin_season = original.begin_season
    if single:
        Message().send_channel_msg(expected_scope=scope, channel=channel, title=media.get_title_vote_string(),
                                   text=media.get_overview_string(), image=media.get_message_image(),
                                   url=media.get_detail_url(), user_id=user_id)
    __search_media(channel, media, user_id, user_name, scope, choice)


def __search_media(in_from, media_info, user_id, user_name, scope, choice):
    """
    开始搜索和发送消息
    """
    # 检查是否存在，电视剧返回不存在的集清单
    exist_flag, no_exists, messages = Downloader().check_exists_medias(meta_info=media_info)
    if messages:
        Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                   title="\n".join(messages),
                                   user_id=user_id)
    # Only a verified gap can start a search or an automatic subscription.
    if exist_flag is not False:
        return

    # 开始搜索
    Message().send_channel_msg(expected_scope=scope, channel=in_from,
                               title="开始搜索 %s ..." % media_info.title,
                               user_id=user_id)
    try:
        search_result, no_exists, search_count, download_count = Searcher().search_one_media(
            media_info=media_info, in_from=in_from, no_exists=no_exists,
            sites=media_info.search_sites, user_name=user_name)
    except (ResourceFilterUnavailable, SearchResultsUnavailable) as err:
        ProgressHelper().end(ProgressKey.Search)
        Message().send_channel_msg(expected_scope=scope, channel=in_from, title=str(err), user_id=user_id)
        return
    # 没有搜索到数据
    if not search_count:
        Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                   title="%s 未搜索到任何资源" % media_info.title,
                                   user_id=user_id)
    else:
        # 搜索到了但是没开自动下载
        if download_count is None:
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title="%s 共搜索到%s个资源，点击选择下载" % (media_info.title, search_count),
                                       image=media_info.get_message_image(),
                                       url="search",
                                       user_id=user_id)
            return
        else:
            # 搜索到了但是没下载到数据
            if download_count == 0:
                Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                           title="%s 共搜索到%s个结果，但没有下载到任何资源" % (
                                               media_info.title, search_count),
                                           user_id=user_id)
    # 没有下载完成，且打开了自动添加订阅
    if not search_result and Config().get_config('pt').get('search_no_result_rss'):
        # 添加订阅
        __rss_media(in_from=in_from,
                    media_info=media_info,
                    user_id=user_id,
                    state='R',
                    user_name=user_name, scope=scope, choice=choice)


def __rss_media(in_from, media_info, user_id=None, state='D', user_name=None, scope=None, choice=None):
    """
    开始添加订阅和发送消息
    """
    # 添加订阅
    mediaid = f"DB:{media_info.douban_id}" if media_info.douban_id else media_info.tmdb_id
    if choice:
        mediaid = choice["selector"]
    code, msg, saved_media = Subscribe().add_rss_subscribe(mtype=media_info.type,
                                                          name=media_info.title,
                                                          year=media_info.year,
                                                          channel=RssType.Auto,
                                                          season=media_info.begin_season,
                                                          mediaid=mediaid,
                                                          state=state,
                                                          rss_sites=media_info.rss_sites,
                                                          search_sites=media_info.search_sites,
                                                          download_setting=media_info.download_setting,
                                                          in_from=in_from,
                                                          user_name=user_name, chat_choice=choice)
    if code == 4:
        return
    if code == 0:
        log.info("【Web】%s %s 已添加订阅" % (saved_media.type.value, saved_media.get_title_string()))
    else:
        if in_from in Message().get_search_types():
            log.info("【Web】%s 添加订阅失败：%s" % (media_info.title, msg))
            Message().send_channel_msg(expected_scope=scope, channel=in_from,
                                       title="%s 添加订阅失败：%s" % (media_info.title, msg),
                                       user_id=user_id)
