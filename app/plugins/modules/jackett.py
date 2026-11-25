from app.plugins.modules._base import _IndexerPlugin


class Jackett(_IndexerPlugin):
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
