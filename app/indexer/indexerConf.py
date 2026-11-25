import json
import subprocess

from config import run_go_helper


class IndexerConf(object):

    def __init__(self,
                 datas=None,
                 siteid=None,
                 cookie=None,
                 name=None,
                 rule=None,
                 public=None,
                 proxy=False,
                 parser=None,
                 ua=None,
                 render=None,
                 builtin=True,
                 language=None,
                 pri=None):
        if not datas:
            return None
        # ID
        self.id = datas.get('id')
        # 站点ID
        self.siteid = siteid
        # 名称
        self.name = datas.get('name') if not name else name
        # 是否内置站点
        self.builtin = datas.get('builtin')
        # 域名
        self.domain = datas.get('domain')
        # 搜索
        self.search_config = datas.get('search', {})
        # 批量搜索，如果为空对象则表示不支持批量搜索
        self.batch = self.search_config.get("batch", {}) if builtin else {}
        # 解析器
        self.parser = parser if parser is not None else datas.get('parser')
        # 是否启用渲染
        self.render = render if render is not None else datas.get("render")
        # 浏览
        self.browse = datas.get('browse', {})
        # 种子过滤
        self.torrents = datas.get('torrents', {})
        # 分类
        self.category = datas.get('category', {})
        # Cookie
        self.cookie = cookie
        # User-Agent
        self.ua = ua
        # 过滤规则
        self.rule = rule
        # 是否公开站点
        self.public = datas.get('public') if not public else public
        # 是否使用代理
        self.proxy = datas.get('proxy') if not proxy else proxy
        # 仅支持的特定语种
        self.language = language
        # 索引器优先级
        self.pri = pri if pri else 0

    def to_dict(self):
        return {
            "id": self.id or "",
            "siteid": self.siteid or "",
            "name": self.name or "",
            "builtin": self.builtin or True,
            "domain": self.domain or "",
            "search": self.search_config or "",
            "batch": self.batch or {},
            "parser": self.parser or "",
            "render": self.render or False,
            "browse": self.browse or {},
            "torrents": self.torrents or {},
            "category": self.category or {},
            "cookie": self.cookie or "",
            "ua": self.ua or "",
            "rule": self.rule or "",
            "public": self.public or False,
            "proxy": self.proxy or "",
            "pri": self.pri or 0
        }

    def to_dict_str(self, ensure_ascii=False, formatted=True):
        if formatted:
            return json.dumps(self.to_dict(), ensure_ascii=ensure_ascii, indent=4)
        return json.dumps(self.to_dict(), ensure_ascii=ensure_ascii)


def plugin_indexers(kind, host, api_key, password=None):
    settings = {"Kind": kind, "Host": host, "APIKey": api_key, "Password": password}
    try:
        raw = run_go_helper("external-indexer", "discover",
                            payload=json.dumps({"Config": settings}).encode(), timeout=35)
        indexers = []
        for row in json.loads(raw):
            indexer = IndexerConf({"id": row["ID"], "name": row["Name"], "domain": row["Domain"],
                                   "public": row["Public"], "builtin": False, "proxy": True,
                                   "parser": kind})
            indexers.append(indexer)
        return indexers
    except (OSError, ValueError, subprocess.SubprocessError):
        return []


def plugin_search(kind, host, api_key, indexer, keyword, page):
    if not indexer or not host or not indexer.domain:
        return None
    try:
        query = {"Config": {"Kind": kind, "Host": host, "APIKey": api_key},
                 "Domain": indexer.domain, "Keyword": keyword or "", "Page": int(page or 0)}
        raw = run_go_helper("external-indexer", "search", payload=json.dumps(query).encode(), timeout=20)
        return json.loads(raw)
    except (OSError, ValueError, TypeError, subprocess.SubprocessError):
        return None
