import subprocess
from threading import Lock

from app.plugins.modules._base import _IPluginModule
from config import go_helper_path


class MteamRssGen(_IPluginModule):
    # 插件名称
    module_name = "馒头大包监控"
    # 插件描述
    module_desc = "后台服务定期监控馒头大包，并发布 RSS"
    # 插件图标
    module_icon = "mteam_rss.png"
    # 主题色
    module_color = "#4179F4"
    # 插件版本
    module_version = "1.1"
    # 插件作者
    module_author = "0xforee"
    # 作者主页
    author_url = "https://github.com/0xforee"
    # 插件配置项ID前缀
    module_config_prefix = "mteam_rss_gen"
    # 加载顺序
    module_order = 0
    # 可使用的用户级别
    auth_level = 2

    def __init__(self):
        self._process = None
        self._lock = Lock()

    def init_config(self, config: dict = None):
        with self._lock:
            if not (config or {}).get("enabled"):
                self._stop()
                return
            if self._process and self._process.poll() is None:
                return
            self._stop()
            process = subprocess.Popen([go_helper_path(), "mteam-rss-server"],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE)
            self._process = process
            try:
                if process.stdout.readline() != b"ready\n":
                    raise RuntimeError("M-Team RSS 服务启动失败，请检查配置和 8001 端口")
            except BaseException:
                self._stop()
                raise
            finally:
                process.stdout.close()

    def get_state(self):
        with self._lock:
            return self._process is not None and self._process.poll() is None

    def stop_service(self):
        with self._lock:
            self._stop()

    def _stop(self):
        if self._process is None:
            return
        process = self._process
        process.stdin.close()
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        self._process = None

    @staticmethod
    def get_fields():
        return [
            {
                'type': 'div',
                'content': [
                    # 同一行
                    [
                        {
                            'title': '开启',
                            'required': "",
                            'tooltip': '开启后会启动 RSS 生成服务，将 http://127.0.0.1:8001/rss_pub.xml 填入馒头刷流任务 RSS 地址使用',
                            'type': 'switch',
                            'id': 'enabled',
                        },
                    ]
                ]
            },
            {
                'type': 'div',
                'content': [
                    # 同一行
                    [
                        {
                            'title': '种子体积最小值（GB）',
                            'required': "required",
                            'tooltip': '大于配置值的种子才会被监控到，默认为 0',
                            'type': 'text',
                            'content': [
                                {
                                    'id': 'torrent_min_size',
                                    'placeholder': '0',
                                }
                            ]
                        },
                    ]
                ]
            },
            {
                'type': 'details',
                'summary': '监控类别',
                'required': "required",
                'tooltip': '监控选中的类别，必选！！',
                'content': [
                    # 同一行
                    [
                        {
                            'id': 'watch_categories',
                            'type': 'form-selectgroup',
                            'content': {
                                'adult': {
                                    'id': 'adult',
                                    'radio': True,
                                    'name': '成人',
                                },
                                'normal': {
                                    'id': 'normal',
                                    'name': '综合',
                                },
                                'movie': {
                                    'id': 'movie',
                                    'name': '电影',
                                },
                                'tvshow': {
                                    'id': 'tvshow',
                                    'name': '剧集',
                                }
                            }
                        },
                    ]
                ]
            },
        ]
