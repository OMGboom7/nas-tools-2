# NAS媒体库管理工具

> React + Go 渐进重构正在 `codex/react-go-refactor` 分支进行，开发方式与迁移约定见 [迁移文档](docs/migration/react-go.md)。

本地开发时，在两个终端分别运行 `make backend-dev` 和 `make frontend-dev`。Go 默认独立运行，首次登录使用仓库下 `.nastool-dev/config.yaml` 中生成的账户和密码。

新架构生产镜像可通过 `make production-build` 构建，或使用 `make production-up` 启动。该镜像由 Go 接管外部 Web 端口并托管 React，Python 服务仅在容器内部提供兼容接口；部署与回退步骤见 [生产切换说明](docs/migration/react-go.md#生产部署与回退)。

另有不含 Python 的 Go-only 候选镜像（`make go-only-build`），用于迁移回归测试。它对尚未迁移的接口明确报错，目前不能替代完整生产镜像；覆盖范围和使用限制见 [Python 退出计划](docs/migration/python-exit.md)。


[![GitHub stars](https://img.shields.io/github/stars/0xforee/nas-tools?style=plastic)](https://github.com/0xforee/nas-tools/stargazers)
[![GitHub forks](https://img.shields.io/github/forks/0xforee/nas-tools?style=plastic)](https://github.com/0xforee/nas-tools/network/members)
[![GitHub issues](https://img.shields.io/github/issues/0xforee/nas-tools?style=plastic)](https://github.com/0xforee/nas-tools/issues)
[![GitHub license](https://img.shields.io/github/license/0xforee/nas-tools?style=plastic)](https://github.com/0xforee/nas-tools/blob/master/LICENSE.md)
[![Docker pulls](https://img.shields.io/docker/pulls/0xforee/nas-tools?style=plastic)](https://hub.docker.com/r/0xforee/nas-tools)
[![Platform](https://img.shields.io/badge/platform-amd64/arm64-pink?style=plastic)](https://hub.docker.com/r/0xforee/nas-tools)

Docker：https://hub.docker.com/repository/docker/0xforee/nas-tools  

TG 交流群：https://t.me/nastoolsolder  
TG 通知频道：https://t.me/ntolder_notify


## 功能： 

1. 优化用户认证
2. 优化新手刷流体验
1. 刷流任务优化：
   * 增加部分下载能力（拆包）
   * 增加限免到期检测能力
   * 刷流界面增加详细信息展示
3. 支持 BT 能力和内置 BT 站点，可以索引和下载 BT 磁链和种子文件
4. 支持 jackett 和 prowlarr 索引器
5. 增加一些入口的快捷跳转能力
6. 完美支持 Mteam 新架构

详细参考 [这里](diff.md)。  

## 安装
### 1、Docker
```
docker pull 0xforee/nas-tools:latest
```
教程见 [这里](docker/readme.md) 。

更新时拉取新镜像，使用原配置卷重新创建容器。

### 2、本地运行
兼容启动器需要 Python 3.10、Go 1.24 或更新版本和 Make。从本项目源码目录构建与当前版本匹配的数据库初始化程序：
```
git submodule update --init --recursive
make db-build
python3 -m pip install cython
python3 -m pip install -r requirements.txt
export NASTOOL_CONFIG="/xxx/config/config.yaml"
nohup python3 run.py & 
```
## 常见问题
请参考 [常见问题](Q&A.md)

## 免责声明
1) 本软件不提供任何内容，仅作为辅助工具简化用户手工操作，对用户的行为及内容毫不知情，使用本软件产生的任何责任需由使用者本人承担。
2) 本软件代码开源，基于开源代码进行修改，人为去除相关限制导致软件被分发、传播并造成责任事件的，需由代码修改发布者承担全部责任。同时按AGPL-3.0开源协议要求，基于此软件代码的所有修改必须开源。
3) 所有搜索结果均来自源站，本软件不承担任何责任
3) 本软件仅供学习交流，请保持低调，勿公开传播
