import json
import subprocess
from contextlib import ExitStack, contextmanager
from threading import local

from app.helper import DbHelper
from app.media.meta import ReleaseGroupsMatcher
from app.utils import StringUtils
from app.utils.commons import singleton
from app.utils.types import MediaType
from config import go_helper_path, run_go_helper


class ResourceFilterUnavailable(RuntimeError):
    pass


_sessions = local()


@contextmanager
def resource_filter_batch():
    if hasattr(_sessions, "stack"):
        yield
        return
    with ExitStack() as stack:
        _sessions.stack = stack
        _sessions.process = None
        try:
            yield
        finally:
            del _sessions.stack, _sessions.process


def _filter_call(op, **query):
    try:
        frame = json.dumps(dict(query, op=op), ensure_ascii=False, allow_nan=False).encode() + b"\n"
        if len(frame) > 512 * 1024:
            raise ResourceFilterUnavailable("资源筛选数据超过单条限制")
        if not hasattr(_sessions, "stack"):
            reply = run_go_helper("filter-resources", payload=frame)
        else:
            process = _sessions.process
            if process is None:
                process = _sessions.stack.enter_context(subprocess.Popen(
                    [go_helper_path(), "filter-resources"], stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL))
                _sessions.process = process
            process.stdin.write(frame)
            process.stdin.flush()
            reply = process.stdout.readline()
        result = json.loads(reply)
        if isinstance(result, dict) and result.get("error"):
            raise ResourceFilterUnavailable("资源筛选暂不可用，请检查过滤规则后重试")
        return result
    except (OSError, ValueError, TypeError, subprocess.SubprocessError) as err:
        raise ResourceFilterUnavailable("资源筛选暂不可用，请检查过滤规则后重试") from err


def _group_id(value):
    try:
        return int(value or 0)
    except (TypeError, ValueError) as err:
        raise ResourceFilterUnavailable("过滤规则组编号有误") from err


def _factor(value):
    return float(value) if value is not None and value != "" else None


@singleton
class Filter:
    rg_matcher = None
    dbhelper = None
    _groups = []
    _rules = []

    def __init__(self):
        self.init_config()

    def init_config(self):
        self.dbhelper = DbHelper()
        self.rg_matcher = ReleaseGroupsMatcher()
        self._groups = self.get_filter_group()
        self._rules = self.get_filter_rule()

    def get_rule_groups(self, groupid=None, default=False):
        """
        获取所有规则组
        """
        ret_groups = []
        for group in self._groups:
            group_info = {
                "id": group.ID,
                "name": group.GROUP_NAME,
                "default": group.IS_DEFAULT,
                "note": group.NOTE
            }
            if (groupid and str(groupid) == str(group.ID)) \
                    or (default and group.IS_DEFAULT == "Y"):
                return group_info
            ret_groups.append(group_info)
        if groupid or default:
            return {}
        return ret_groups

    def get_rule_infos(self):
        """
        获取所有的规则组及组内的规则
        """
        groups = self.get_rule_groups()
        for group in groups:
            group['rules'] = self.get_rules(group.get("id"))
        return groups

    def get_rules(self, groupid, ruleid=None):
        """
        获取过滤规则
        """
        if not groupid:
            return []
        ret_rules = []
        for rule in self._rules:
            rule_info = {
                "id": rule.ID,
                "group": rule.GROUP_ID,
                "name": rule.ROLE_NAME,
                "pri": rule.PRIORITY or 0,
                "include": rule.INCLUDE.split("\n") if rule.INCLUDE else [],
                "exclude": rule.EXCLUDE.split("\n") if rule.EXCLUDE else [],
                "size": rule.SIZE_LIMIT,
                "free": rule.NOTE,
                "free_text": {
                    "1.0 1.0": "普通",
                    "1.0 0.0": "免费",
                    "2.0 0.0": "2X免费"
                }.get(rule.NOTE, "全部") if rule.NOTE else ""
            }
            if str(rule.GROUP_ID) == str(groupid) \
                    and (not ruleid or int(ruleid) == rule.ID):
                ret_rules.append(rule_info)
        if ruleid:
            return ret_rules[0] if ret_rules else {}
        return ret_rules

    def get_rule_first_order(self, rulegroup):
        return _filter_call("highest", group=_group_id(rulegroup))

    def is_rule_free(self, rulegroup=None):
        return _filter_call("needsPromotion", group=_group_id(rulegroup))

    @staticmethod
    def is_torrent_match_sey(media_info, s_num, e_num, year_str):
        def numbers(value):
            if value is None or value == "":
                return []
            return [int(item) for item in (value if isinstance(value, (list, tuple)) else [value])]
        return _filter_call("scope", scope={
            "wantedSeasons": numbers(s_num), "wantedEpisodes": numbers(e_num),
            "seasons": numbers(media_info.get_season_list()) if s_num is not None else [],
            "episodes": numbers(media_info.get_episode_list()) if e_num is not None else [],
            "wantedYear": str(year_str or ""), "year": str(media_info.year or ""),
        })

    def _match(self, media, args, upload=None, download=None, require_known=True):
        size = StringUtils.num_filesize(media.size)
        decision = _filter_call("match", filter={
            "group": _group_id(args.get("rule")), "original": media.org_string,
            "metadata": {"title": media.rev_string, "subtitle": media.subtitle,
                         "movie": media.type == MediaType.MOVIE, "episodes": int(media.total_episodes or 0),
                         "sizeBytes": size, "uploadFactor": _factor(media.upload_volume_factor),
                         "downloadFactor": _factor(media.download_volume_factor), "requireKnown": require_known},
            "quality": args.get("restype"), "edition": media.get_edtion_string() if args.get("restype") else "",
            "resolution": args.get("pix"), "pixels": media.resource_pix,
            "team": args.get("team"), "releaseGroup": media.resource_team,
            "separator": self.rg_matcher.custom_separator,
            "include": args.get("include"), "exclude": args.get("exclude"), "keyword": args.get("key"),
            "promotion": args.get("sp_state"), "upload": _factor(upload), "download": _factor(download),
        })
        if decision["SizeChecked"]:
            media.size = size
        if decision["Team"]:
            media.resource_team = decision["Team"]
        return decision

    def check_rules(self, meta_info, rulegroup=None):
        if not meta_info:
            return False, 0, ""
        result = self._match(meta_info, {"rule": rulegroup}, require_known=False)
        return result["Matched"], result["Order"], result["GroupName"]

    def check_torrent_filter(self, meta_info, filter_args, uploadvolumefactor=None, downloadvolumefactor=None):
        result = self._match(meta_info, filter_args, uploadvolumefactor, downloadvolumefactor)
        reason = result["Reason"]
        if not reason:
            source = "订阅/站点" if filter_args.get("rule") else "默认"
            reason = (f"{meta_info.org_string} 大小：{StringUtils.str_filesize(meta_info.size)} "
                      f"促销：{meta_info.get_volume_factor_string()} 不符合{source}过滤规则 {result['GroupName']} 要求")
        return result["Matched"], result["Order"], reason

    def add_group(self, name, default='N'):
        """
        添加过滤规则组
        """
        ret = self.dbhelper.add_filter_group(name, default)
        self.init_config()
        return ret

    def delete_filtergroup(self, groupid):
        """
        删除过滤规则组
        """
        ret = self.dbhelper.delete_filtergroup(groupid)
        self.init_config()
        return ret

    def set_default_filtergroup(self, groupid):
        """
        设置默认过滤规则组
        """
        ret = self.dbhelper.set_default_filtergroup(groupid)
        self.init_config()
        return ret

    def add_filter_rule(self, item, ruleid=None):
        """
        添加过滤规则
        """
        ret = self.dbhelper.insert_filter_rule(item, ruleid)
        self.init_config()
        return ret

    def delete_filterrule(self, ruleid):
        """
        删除过滤规则
        """
        ret = self.dbhelper.delete_filterrule(ruleid)
        self.init_config()
        return ret

    def get_filter_group(self, gid=None):
        """
        获取过滤规则组
        """
        return self.dbhelper.get_config_filter_group(gid)

    def get_filter_rule(self, groupid=None):
        """
        获取过滤规则
        """
        return self.dbhelper.get_config_filter_rule(groupid)

    def get_filter_groupid_by_name(self, name):
        """
        根据名称获取过滤规则组ID
        """
        return self.dbhelper.get_filter_groupid_by_name(name)
