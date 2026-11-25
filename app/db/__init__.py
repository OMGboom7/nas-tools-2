import log
from .main_db import MainDb
from .main_db import DbPersist
from .media_db import MediaDb


def init_data():
    """
    初始化数据
    """
    log.console('开始初始化数据...')
    MainDb().init_data()
    log.console('数据初始化完成')
