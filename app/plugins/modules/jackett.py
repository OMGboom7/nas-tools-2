from datetime import datetime, timedelta
from threading import Event
from jinja2 import Template

import pytz
from apscheduler.schedulers.background import BackgroundScheduler
from apscheduler.triggers.cron import CronTrigger

from app.indexer.indexerConf import plugin_indexers, plugin_search
from app.utils import StringUtils

from app.plugins import EventManager
from app.plugins.modules._base import _IPluginModule
from config import Config

class Jackett(_IPluginModule):
    # 插件名称
    module_name = "Jackett"
    # 插件描述
    module_desc = "让内荐索引器支持检索Jackett站点资源"
    # 插件图标
    module_icon = "jackett.png"
    # 主题色
    module_color = "#141A21"
    # 插件版本
    module_version = "1.5"
    # 插件作者
    module_author = "hsuyelin"
    # 作者主页
    author_url = "https://github.com/hsuyelin"
    # 插件配置项ID前缀
    module_config_prefix = "jackett_"
    # 加载顺序
    module_order = 15
    # 可使用的用户级别
    auth_level = 1

    # 私有属性
    eventmanager = None
    _scheduler = None
    _cron = None
    _enable = False
    _host = ""
    _api_key = ""
    _password = ""
    _onlyonce = False
    _sites = None

    # 退出事件
    _event = Event()

    @staticmethod
    def get_fields():
        return [
            # 同一板块
            {
                'type': 'div',
                'content': [
                    # 同一行
                    [
                        {
                            'title': 'Jackett地址',
                            'required': "required",
                            'tooltip': 'Jackett访问地址和端口，如为https需加https://前缀。注意需要先在Jackett中添加indexer，才能正常测试通过和使用',
                            'type': 'text',
                            'content': [
                                {
                                    'id': 'host',
                                    'placeholder': 'http://127.0.0.1:9117',
                                }
                            ]
                        },
                        {
                            'title': 'Api Key',
                            'required': "required",
                            'tooltip': 'Jackett管理界面右上角复制API Key',
                            'type': 'text',
                            'content': [
                                {
                                    'id': 'api_key',
                                    'placeholder': '',
                                }
                            ]
                        }
                    ],
                    [
                        {
                            'title': '密码',
                            'required': "required",
                            'tooltip': 'Jackett管理界面中配置的Admin password，如未配置可为空',
                            'type': 'password',
                            'content': [
                                {
                                    'id': 'password',
                                    'placeholder': '',
                                }
                            ]
                        }
                    ],
                    [
                        {
                            'title': '更新周期',
                            'required': "",
                            'tooltip': '索引列表更新周期，支持5位cron表达式，默认每24小时运行一次',
                            'type': 'text',
                            'content': [
                                {
                                    'id': 'cron',
                                    'placeholder': '0 0 */24 * *',
                                }
                            ]
                        }
                    ],
                    [
                        {
                            'title': '立即运行一次',
                            'required': "",
                            'tooltip': '打开后立即运行一次获取索引器列表，否则需要等到预先设置的更新周期才会获取',
                            'type': 'switch',
                            'id': 'onlyonce'
                        }
                    ]
                ]
            },
        ]

    def get_page(self):
        """
        插件的额外页面，返回页面标题和页面内容
        :return: 标题，页面内容，确定按钮响应函数
        """
        if not isinstance(self._sites, list) or len(self._sites) <= 0:
            return None, None, None
        template = """
          <div class="table-responsive table-modal-body">
            <table class="table table-vcenter card-table table-hover table-striped">
              <thead>
              {% if IndexersCount > 0 %}
              <tr>
                <th>id</th>
                <th>索引</th>
                <th>是否公开</th>
                <th></th>
              </tr>
              {% endif %}
              </thead>
              <tbody>
              {% if IndexersCount > 0 %}
                {% for Item in Indexers %}
                  <tr id="indexer_{{ Item.id }}">
                    <td>{{ Item.id }}</td>
                    <td>{{ Item.domain }}</td>
                    <td>{{ Item.public }}</td>
                  </tr>
                {% endfor %}
              {% endif %}
              </tbody>
            </table>
          </div>
        """
        return "索引列表", Template(template).render(IndexersCount=len(self._sites), Indexers=self._sites), None

    def init_config(self, config=None):
        self.eventmanager = EventManager()

        # 读取配置
        if config:
            self._host = config.get("host")
            if self._host:
                if not self._host.startswith('http'):
                    self._host = "http://" + self._host
                if self._host.endswith('/'):
                    self._host = self._host.rstrip('/')
            self._api_key = config.get("api_key")
            self._password = config.get("password")
            self._enable = self.get_status()
            self._onlyonce = config.get("onlyonce")
            self._cron = config.get("cron")
            if not StringUtils.is_string_and_not_empty(self._cron):
                self._cron = "0 0 */24 * *"

        # 停止现有任务
        self.stop_service()

        # 启动定时任务 & 立即运行一次
        if self._onlyonce:
            self._scheduler = BackgroundScheduler(timezone=Config().get_timezone())

            if self._cron:
                self.info(f"【{self.module_name}】 索引更新服务启动，周期：{self._cron}")
                self._scheduler.add_job(self.get_status, CronTrigger.from_crontab(self._cron))

            if self._onlyonce:
                self.info(f"【{self.module_name}】开始获取索引器状态")
                self._scheduler.add_job(self.get_status, 'date',
                                        run_date=datetime.now(tz=pytz.timezone(Config().get_timezone())) + timedelta(
                                            seconds=3))
                # 关闭一次性开关
                self._onlyonce = False
                self.__update_config()

            if self._cron or self._onlyonce:
                # 启动服务
                self._scheduler.print_jobs()
                self._scheduler.start()

    def get_status(self):
        """
        检查连通性
        :return: True、False
        """
        if not self._api_key or not self._host:
            return False
        self._sites = self.get_indexers()
        return True if isinstance(self._sites, list) and len(self._sites) > 0 else False

    def get_state(self):
        return self._enable

    def stop_service(self):
        """
        退出插件
        """
        try:
            if self._scheduler:
                self._scheduler.remove_all_jobs()
                if self._scheduler.running:
                    self._event.set()
                    self._scheduler.shutdown()
                    self._event.clear()
                self._scheduler = None
        except Exception as e:
            self.error(f"【{self.module_name}】停止插件错误: {str(e)}")

    def __update_config(self):
        """
        更新优选插件配置
        """
        self.update_config({
            "onlyonce": False,
            "cron": self._cron,
            "host": self._host,
            "api_key": self._api_key,
            "password": self._password,
        })

    def get_indexers(self):
        return plugin_indexers(self.module_name, self._host, self._api_key, self._password)

    def search(self, indexer, keyword, page):
        return plugin_search(self.module_name, self._host, self._api_key, indexer, keyword, page)
