import json
import os
import re
import subprocess
import tempfile
from pathlib import Path
from urllib.parse import unquote, urlparse

import log
from app.utils import StringUtils
from app.utils.mteam_utils import MteamUtils
from app.utils.http_utils import RequestUtils
from app.utils.types import MediaType
from config import Config, run_go_helper


class Torrent:
    _torrent_temp_path = None

    def __init__(self):
        self._torrent_temp_path = Config().get_temp_path()
        if not os.path.exists(self._torrent_temp_path):
            os.makedirs(self._torrent_temp_path, exist_ok=True)

    def get_torrent_info(self, url, cookie=None, ua=None, referer=None, proxy=False):
        """
        把种子下载到本地，返回种子内容
        :param url: 种子链接
        :param cookie: 站点Cookie
        :param ua: 站点UserAgent
        :param referer: 关联地址，有的网站需要这个否则无法下载
        :param proxy: 是否使用内置代理
        :return: 种子保存路径、种子内容、种子文件列表主目录、种子文件列表、错误信息
        """
        if not url:
            return None, None, "", [], "URL为空"
        if url.startswith("magnet:"):
            return None, url, "", [], "磁力链接"
        try:
            return self._download(url, cookie, ua, referer, proxy)
        except (OSError, ValueError, subprocess.SubprocessError):
            return None, None, "", [], "读取或解析种子文件失败"

    def save_torrent_file(self, url, cookie=None, ua=None, referer=None, proxy=False):
        path, content, _, _, error = self.get_torrent_info(url, cookie, ua, referer, proxy)
        return path, content, error

    def _download(self, url, cookie, ua, referer, proxy):
        try:
            host = (urlparse(url).hostname or "").lower()
        except ValueError:
            return None, None, "", [], "种子链接无效"
        if host in ("kp.m-team.info", "api.m-team.info", "m-team.cc", "m-team.io") or host.endswith((".m-team.cc", ".m-team.io")):
            path, content, message = self.__download_mteam(url, ua, proxy)
            if not path:
                return None, content, "", [], message
            folder, names = self._layout(content)
            return path, content, folder, names, ""
        req = RequestUtils(
            headers=ua,
            cookies=cookie,
            referer=referer,
            proxies=Config().get_proxies() if proxy else None
        ).get_res(url=url, allow_redirects=False)
        redirects = 0
        while req and req.status_code in (301, 302):
            url = req.headers.get("Location")
            redirects += 1
            if not url or redirects > 10:
                return None, None, "", [], "无效的种子下载重定向"
            if url and url.startswith("magnet:"):
                return None, url, "", [], "磁力链接"
            req = RequestUtils(
                headers=ua,
                cookies=cookie,
                referer=referer,
                proxies=Config().get_proxies() if proxy else None
            ).get_res(url=url, allow_redirects=False)
        if req and req.status_code == 200:
            if not req.content:
                return None, None, "", [], "未下载到种子数据"
            # 解析内容格式
            if req.content.startswith(b"magnet:"):
                # 磁力链接
                return None, req.text, "", [], "磁力链接"
            elif not req.content.startswith(b"d") and "下载种子文件" in req.text:
                # 首次下载提示页面
                skip_flag = False
                try:
                    form = re.findall(r'<form.*?action="(.*?)".*?>(.*?)</form>', req.text, re.S)
                    if form:
                        action = form[0][0]
                        if not action or action == "?":
                            action = url
                        elif not action.startswith('http'):
                            action = StringUtils.get_base_url(url) + action
                        inputs = re.findall(r'<input.*?name="(.*?)".*?value="(.*?)".*?>', form[0][1], re.S)
                        if action and inputs:
                            data = {}
                            for item in inputs:
                                data[item[0]] = item[1]
                            # 改写req
                            req = RequestUtils(
                                headers=ua,
                                cookies=cookie,
                                referer=referer,
                                proxies=Config().get_proxies() if proxy else None
                            ).post_res(url=action, data=data)
                            if req and req.status_code == 200:
                                skip_flag = True
                            elif req is not None:
                                log.warn(f"【Downloader】触发了站点首次种子下载，且无法自动跳过，"
                                         f"返回码：{req.status_code}，错误原因：{req.reason}")
                            else:
                                log.warn("【Downloader】无法通过站点首次种子下载提示")
                except Exception:
                    log.warn("【Downloader】处理站点首次种子下载提示失败")

                if not skip_flag:
                    return None, None, "", [], "种子数据有误，请确认链接是否正确，如为PT站点则需手工在站点下载一次种子"
            content = req.content
            folder, names = self._layout(content)
            filename = self.__get_url_torrent_filename(req, url) or folder or names[0]
            path = self._save_content(self._torrent_temp_path, filename, content)
            return path, content, folder, names, ""
        elif req is None:
            return None, None, "", [], "无法打开种子链接"
        elif req.status_code == 429:
            return None, None, "", [], "触发站点流控，请稍后重试"
        else:
            return None, None, "", [], "下载种子出错，状态码：%s" % req.status_code

    def __download_mteam(self, url, ua, proxy):
        try:
            path = urlparse(url).path
            key = MteamUtils.get_api_key(url) if path.startswith("/detail/") else None
            proxy_url = (Config().get_proxies() or {}).get("https") if proxy else None
            if proxy and not proxy_url:
                return None, None, "未配置站点代理"
            query = {"url": url, "key": key, "ua": ua or Config().get_ua(),
                     "proxy": proxy_url, "directory": os.path.abspath(self._torrent_temp_path)}
            result = json.loads(run_go_helper("mteam-download", payload=json.dumps(query).encode(), timeout=45))
            if result.get("magnet"):
                return None, result["magnet"], "磁力链接"
            if result.get("status") == 429:
                return None, None, "触发站点流控，请稍后重试"
            if "status" in result:
                return None, None, f"下载种子出错，状态码：{result['status']}"
            file_path = result["path"]
            return file_path, self._read_bytes(file_path), ""
        except (KeyError, OSError, ValueError, subprocess.SubprocessError):
            return None, None, "M-Team 种子下载失败"

    @staticmethod
    def _read_bytes(path):
        with open(path, "rb") as source:
            content = source.read((8 << 20) + 1)
        if len(content) > 8 << 20:
            raise ValueError("种子文件过大")
        return content

    @staticmethod
    def _layout(content):
        layout = json.loads(run_go_helper("torrent-metadata", "layout", payload=content))
        return layout["folder"], layout["files"]

    @staticmethod
    def _save_content(directory, filename, content):
        name = Path(filename).name
        if not name or name in (".", "..") or re.search(r'[\x00-\x1f<>:"/\\|?*]', name) or len(name.encode()) > 200:
            name = "download.torrent"
        if not name.lower().endswith(".torrent"):
            name += ".torrent"
        path = os.path.join(directory, name)
        try:
            target = os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb")
        except FileExistsError:
            target = tempfile.NamedTemporaryFile(dir=directory, prefix=name[:-8] + "-", suffix=".torrent", delete=False)
            path = target.name
        try:
            with target:
                target.write(content)
        except OSError:
            os.unlink(path)
            raise
        return path

    @staticmethod
    def get_torrent_files(path):
        _, folder, files, error = Torrent.read_torrent_content(path)
        return folder, files, error

    @staticmethod
    def read_torrent_content(path):
        if not path:
            return None, "", [], "种子文件不存在"
        try:
            content = Torrent._read_bytes(path)
            folder, files = Torrent._layout(content)
            return content, folder, files, ""
        except (OSError, ValueError, subprocess.SubprocessError):
            return None, "", [], "读取或解析种子文件失败"

    @staticmethod
    def __get_url_torrent_filename(req, url):
        """
        从下载请求中获取种子文件名
        """
        if not req:
            return ""
        disposition = req.headers.get('content-disposition') or ""
        file_name = re.findall(r"filename=\"?(.+)\"?", disposition)
        if file_name:
            file_name = unquote(file_name[0].split(";")[0].strip())
            try:
                file_name = file_name.encode("latin-1").decode("utf-8")
            except UnicodeError:
                pass
            if file_name.endswith('"'):
                file_name = file_name[:-1]
        elif url and url.endswith(".torrent"):
            file_name = unquote(url.split("/")[-1])
        else:
            file_name = ""
        return file_name.replace('/', '')

    @staticmethod
    def get_intersection_episodes(target, source, title):
        """
        对两个季集字典进行判重，有相同项目的取集的交集
        """
        if not source or not title:
            return target
        if not source.get(title):
            return target
        if not target.get(title):
            target[title] = source.get(title)
            return target
        index = -1
        for target_info in target.get(title):
            index += 1
            source_info = None
            for info in source.get(title):
                if info.get("season") == target_info.get("season"):
                    source_info = info
                    break
            if not source_info:
                continue
            if not source_info.get("episodes"):
                continue
            if not target_info.get("episodes"):
                target_episodes = source_info.get("episodes")
                target[title][index]["episodes"] = target_episodes
                continue
            target_episodes = list(set(target_info.get("episodes")).intersection(set(source_info.get("episodes"))))
            target[title][index]["episodes"] = target_episodes
        return target

    @staticmethod
    def get_download_list(media_list, download_order):
        """
        对媒体信息进行排序、去重
        """
        if not media_list:
            return []

        # 排序函数，标题、站点、资源类型、做种数量
        def get_sort_str(x):
            season_len = str(len(x.get_season_list())).rjust(2, '0')
            episode_len = str(len(x.get_episode_list())).rjust(4, '0')
            # 排序：标题、资源类型、站点、做种、季集
            if download_order == "seeder":
                return "%s%s%s%s%s" % (str(x.title).ljust(100, ' '),
                                       str(x.res_order).rjust(3, '0'),
                                       str(x.seeders).rjust(10, '0'),
                                       str(x.site_order).rjust(3, '0'),
                                       "%s%s" % (season_len, episode_len))
            else:
                return "%s%s%s%s%s" % (str(x.title).ljust(100, ' '),
                                       str(x.res_order).rjust(3, '0'),
                                       str(x.site_order).rjust(3, '0'),
                                       str(x.seeders).rjust(10, '0'),
                                       "%s%s" % (season_len, episode_len))

        # 匹配的资源中排序分组选最好的一个下载
        # 按站点顺序、资源匹配顺序、做种人数下载数逆序排序
        media_list = sorted(media_list, key=lambda x: get_sort_str(x), reverse=True)
        # 控重
        can_download_list_item = []
        can_download_list = []
        # 排序后重新加入数组，按真实名称控重，即只取每个名称的第一个
        for t_item in media_list:
            # 控重的主链是名称、年份、季、集
            if t_item.type != MediaType.MOVIE:
                media_name = "%s%s" % (t_item.get_title_string(),
                                       t_item.get_season_episode_string())
            else:
                media_name = t_item.get_title_string()
            if media_name not in can_download_list:
                can_download_list.append(media_name)
                can_download_list_item.append(t_item)
        return can_download_list_item

    @staticmethod
    def magent2torrent(url, path, timeout=20):
        try:
            seconds = min(45, int(timeout))
            query = json.dumps({"magnet": url, "seconds": seconds}).encode()
            raw = run_go_helper("torrent-metadata", "resolve", payload=query, timeout=max(seconds, 1) + 5)
            folder, files = Torrent._layout(raw)
            return Torrent._save_content(path, folder or files[0], raw), ""
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            return None, "无法获取磁力链接元数据"

    @staticmethod
    def binary_data_to_magnet_link(binary_data):
        try:
            return run_go_helper("torrent-metadata", "magnet", payload=binary_data).decode()
        except (OSError, ValueError, subprocess.SubprocessError):
            return None

    @staticmethod
    def is_magnet(link):
        """
        判断是否是磁力
        """
        return link.lower().startswith(("magnet:?xt=urn:btih:", "magnet:?xt=urn:btmh:"))

    @staticmethod        
    def maybe_torrent_url(link):
        """
        判断是否可能是种子url
        """
        try:
            parsed = urlparse(link)
            return bool(parsed.netloc) and parsed.scheme in ['http', 'https', 'ftp']
        except Exception as err:
            return False

    @staticmethod
    def format_enclosure(link):
        """
        格式化一个链接
        如果是磁力链接或者为私有PT站点则直接返回
        如果不是磁力链接看是否是种子链接，如果是则下载种子后转换为磁力链接
        """
        if not StringUtils.is_string_and_not_empty(link):
            return None
        if Torrent.is_magnet(link):
            return link
        if not Torrent.maybe_torrent_url(link):
            return None

        _, torrent_content, _, _, retmsg = Torrent().get_torrent_info(link)
        if not torrent_content:
            print(f"下载种子文件出错: {retmsg}")
            return None
        return torrent_content
