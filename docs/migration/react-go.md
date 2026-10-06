# React + Go 渐进迁移

本目录记录 NAS Tools 从 Flask/Jinja2 向 React + Go 迁移的约定和进度。最终删除 Python 的门槛与分批顺序见 [Python 完全退出计划](./python-exit.md)。

## 当前结构

- `frontend/`：React、TypeScript 和 Vite 前端。
- `backend/`：Go API 服务与迁移网关。
- `web/`、`app/`：迁移期间继续运行的 Python 旧系统。

浏览器在开发环境中访问 React 的 `5173` 端口。React 将 `/api` 请求转发到 Go 的 `3001` 端口。Go 直接处理已经迁移的接口，并将其他 `/api/*` 请求转发到 Flask 的 `3000` 端口。

```text
浏览器 :5173 -> Go :3001 -> 已迁移的 Go 接口
                         -> 未迁移接口 -> Flask :3000
```

## 本地启动

首次安装前端依赖：

```bash
make frontend-install
```

分别在三个终端中启动旧服务、Go 网关和 React：

```bash
python3 run.py
make backend-dev
make frontend-dev
```

打开 <http://localhost:5173>。Go 健康检查位于 <http://localhost:3001/api/v1/health>。

## 配置

Go 服务支持以下环境变量：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `NASTOOL_GO_ADDRESS` | `:3001` | Go HTTP 监听地址 |
| `NASTOOL_LEGACY_URL` | `http://127.0.0.1:3000` | Flask 旧服务地址 |
| `NASTOOL_FRONTEND_DIST` | 空 | React 构建产物目录；设置后由 Go 托管前端 |
| `NASTOOL_CONFIG` | 空 | 现有 `config.yaml` 路径；设置后启用 Go 原生认证并读取同目录的 `user.db` |
| `NASTOOL_UI_MODE` | `legacy`（生产镜像为 `react`） | 容器入口模式；`react` 启动 Go 公网关，`legacy` 直接暴露 Flask |
| `NASTOOL_LEGACY_PORT` | `3002` | React 模式下 Flask 的容器内部监听端口 |

## 迁移规则

1. 保持现有 API 路径和响应结构，前端与后端不要在同一步骤中同时改变协议。
2. 每迁移一个接口，都在 Go 中增加自动化测试，再由 Go 接管该路由。
3. 尚未迁移的接口必须继续通过旧服务工作。
4. Python 实现只有在对应 Go 功能完成回归验证后才删除。

## 计划顺序

1. 基础框架、健康检查和兼容代理。（已完成）
2. React 登录、退出及当前用户会话。（已完成；认证、存量密码校验和用户读取已由 Go 原生提供）
3. 权限导航框架、媒体统计、存储空间、继续观看和最新入库。（已完成）
4. 资源搜索。（已完成；搜索执行仍复用旧索引器）
5. 下载动作与下载管理。（已完成；下载器执行仍由旧服务提供）
6. 订阅管理。（已完成列表、历史、刷新、重新订阅和删除）
7. 探索与默认规则新增订阅。（已完成）
8. 订阅高级编辑及其站点、过滤和下载选项。（已完成）
9. 站点管理。（已完成安全列表、用途筛选、单个/批量连接测试、新增、编辑和删除；敏感字段只写不回显）
10. 服务状态与配置。（已完成下载器、媒体服务器和索引器的脱敏概览与连接测试；已完成下载器新增、编辑、默认切换、删除及媒体服务器编辑）
11. 下载目录、通知和插件系统。（下载器目录规则、通知渠道管理，以及插件安全概览、运行状态、安装、卸载、通用配置编辑和结构化扩展页已完成；豆瓣同步、豆瓣榜单、随机电影历史及媒体库归档文件删除均已通过专用接口迁移）
12. 生产构建、部署切换与旧代码清理。

## 生产部署与回退

新架构使用独立的多阶段镜像定义 `docker/production.Dockerfile`：Node 阶段构建 React，Go 阶段生成静态网关二进制，最终 Python Alpine 镜像只保留运行产物和旧服务依赖。本地配置文件、Git 元数据、依赖目录和构建产物均通过 `.dockerignore` 排除，不会意外打包进镜像。

构建并启动：

```bash
make production-build
make production-up
```

也可以直接执行：

```bash
docker compose -f docker/compose.react-go.yml up -d --build
```

React 模式的进程与端口关系如下：

```text
宿主机/浏览器 -> Go :配置中的 app.web_port（默认 3000）
                         -> React 静态文件
                         -> 已迁移的 Go API
                         -> Flask 127.0.0.1:3002（兼容 API）
```

上线前应确认：

1. `/config/config.yaml` 已挂载，并且 `app.web_port` 与 compose 暴露端口一致。
2. `NASTOOL_AUTO_UPDATE=false`。生产镜像是不可变构建，升级应重新构建镜像，避免运行中的源码与 Go/React 产物版本不一致。
3. React + Go 容器入口当前提供 HTTP；如使用 HTTPS，应在 Nginx、Caddy 或其他反向代理终止 TLS。Flask 配置中的 `ssl_cert` 和 `ssl_key` 在内部兼容模式下不会启用。
4. 健康检查访问 `GET /api/v1/health`；Flask 兼容服务必须先就绪，Go 服务随后才会对外标记为可用。

如需立即回退旧界面，将 compose 中的环境变量改为：

```yaml
NASTOOL_UI_MODE: legacy
```

然后重新创建容器。Go 服务会停止接管请求，Flask 恢复监听配置中的原 Web 端口；配置数据库和媒体目录无需迁移。回到 React 时将值恢复为 `react` 即可。不要把 `NASTOOL_LEGACY_PORT` 设置为外部 Web 端口，否则两个服务会发生端口冲突。

## 已迁移的 Go 接口

| 接口 | 状态 | 说明 |
| --- | --- | --- |
| `GET /api/v1/health` | Go 原生 | 服务健康检查 |
| `POST /api/v1/user/login` | Go 原生 | 读取现有 YAML 管理员和 SQLite 用户，兼容 Werkzeug PBKDF2/Scrypt 密码并签发两小时 JWT |
| `POST /api/v1/system/logout` | Go 原生 | 撤销当前 JWT；迁移完成前 Flask 可直接验证 Go 签发的兼容令牌 |
| `POST /api/v1/user/info`、`list`、`manage`、`auth` | Go 原生 | 原生读取和维护 SQLite 用户；用户新增与删除仅管理员可用，旧密码登录后自动升级哈希 |
| `POST /api/v1/config/info`、`update`、`directory` | Go 原生 | 原子读取和更新 YAML，保留注释与权限并生成最近一次备份；认证配置更新后即时重载 |
| `POST /api/v1/config/set` | Go 原生 | 原生写入 `SYSTEM_DICT` 系统设置 |
| `POST /api/v1/download/client/list`、`add`、`delete`、`check` | Go 原生 | 原生维护 SQLite 下载器配置，并由服务页直接读取，不再经 Flask 配置接口 |
| `GET /api/v1/dashboard` | Go 聚合 | 并行聚合旧服务中的媒体统计、空间和继续观看数据 |
| `GET /api/v1/dashboard/image` | Go 原生 | 仅代理由服务端签名的媒体封面地址 |
| `POST /api/v1/search/resources` | Go 聚合 | 启动旧索引器搜索，并将不同站点的结果规范化为 React 使用的统一结构 |
| `GET /api/v1/downloads` | Go 聚合 | 并行读取当前下载任务与近期下载记录，并统一任务字段 |
| `POST /api/v1/downloads/resource` | Go 适配 | 将搜索结果加入现有下载器 |
| `POST /api/v1/downloads/{id}/{action}` | Go 适配 | 开始、暂停或删除下载任务；删除动作由 React 二次确认 |
| `GET /api/v1/subscriptions` | Go 聚合 | 并行读取电影、电视剧订阅及两类订阅历史，并统一状态和进度字段 |
| `POST /api/v1/subscriptions/{type}/{id}/{action}` | Go 适配 | 手动触发订阅搜索或删除当前订阅 |
| `POST /api/v1/subscriptions/history/{type}/{id}/{action}` | Go 适配 | 重新订阅或删除历史记录 |
| `POST /api/v1/discovery` | Go 聚合 | 按趋势、热门或最新分类读取推荐媒体并统一卡片字段 |
| `POST /api/v1/subscriptions` | Go 适配 | 使用媒体信息和现有默认规则新增订阅，也为后续编辑表单保留完整字段 |
| `GET /api/v1/subscriptions/options` | Go 聚合 | 并行读取 RSS/搜索站点、过滤规则、下载设置及保存目录 |
| `GET /api/v1/sites` | Go 适配 | 读取站点列表并仅返回名称、域名、优先级和用途开关，不向浏览器暴露 Cookie、API Key、UA 或完整 RSS 地址 |
| `POST /api/v1/sites/{id}/test` | Go 适配 | 使用服务端保存的凭据测试指定站点连接，并返回结果与耗时 |
| `GET /api/v1/sites/{id}` | Go 适配 | 返回可编辑的安全站点详情；凭据和带密钥的地址仅返回是否已配置 |
| `POST /api/v1/sites` | Go 适配 | 新增站点并校验地址、用途和流控配置 |
| `PUT /api/v1/sites/{id}` | Go 适配 | 在服务端合并站点设置；只写字段支持留空保留、输入替换或显式清除 |
| `DELETE /api/v1/sites/{id}` | Go 适配 | 删除单个站点；React 端要求输入完整站点名称后才能确认 |
| `GET /api/v1/sites/options` | Go 聚合 | 并行读取过滤规则与下载设置，供站点编辑器选择 |
| `GET /api/v1/services` | Go 聚合 | 并行读取下载器、媒体服务器、索引器及媒体统计，并仅返回脱敏状态信息 |
| `POST /api/v1/services/{kind}/{id}/test` | Go 适配 | 在服务端使用保存的凭据测试下载器或媒体服务器；索引器测试其来源加载状态 |
| `GET /api/v1/services/downloader/{id}` | Go 适配 | 返回下载器安全配置；账户、密码、令牌与 Cookie 仅返回是否已配置 |
| `POST /api/v1/services/downloader` | Go 适配 | 新增 qBittorrent、Transmission、Aria2、115 网盘或 PikPak 下载器 |
| `PUT /api/v1/services/downloader/{id}` | Go 适配 | 在服务端合并下载器凭据，并保存媒体类型、二级分类、保存路径、容器路径及 qBittorrent 标签规则 |
| `GET /api/v1/services/downloader-options` | Go 聚合 | 读取电影、电视剧和动漫的二级分类选项，供下载目录规则编辑器使用 |
| `DELETE /api/v1/services/downloader/{id}` | Go 适配 | 删除下载器；React 端要求输入完整名称确认 |
| `POST /api/v1/services/downloader/{id}/default` | Go 适配 | 将指定下载器设为默认下载器 |
| `GET /api/v1/services/media/{id}` | Go 适配 | 返回 Emby、Jellyfin 或 Plex 的安全配置和凭据配置状态 |
| `PUT /api/v1/services/media/{id}` | Go 适配 | 保存媒体服务器配置，可切换当前服务器，并在服务端保留未替换凭据 |
| `GET /api/v1/notifications` | Go 适配 | 返回通知渠道、启用状态与接收事件的脱敏概览，不向浏览器返回渠道凭据 |
| `GET /api/v1/notifications/options` | Go 适配 | 读取全部通知渠道的动态字段、默认值、交互能力和通知事件，用于通用 React 编辑器 |
| `GET /api/v1/notifications/{id}` | Go 适配 | 返回通知渠道安全详情；文本、URL、令牌和模板只返回是否已配置，开关与下拉值可安全回显 |
| `POST /api/v1/notifications` | Go 适配 | 新增 Telegram、微信、Webhook 等现有消息模块支持的通知渠道 |
| `PUT /api/v1/notifications/{id}` | Go 适配 | 在服务端合并只写配置，支持保留、替换或清除字段，并校验必填项、下拉值和接收事件 |
| `PUT /api/v1/notifications/{id}/status` | Go 适配 | 开关通知渠道或交互能力；同类型交互渠道的互斥逻辑仍由旧消息服务执行 |
| `POST /api/v1/notifications/{id}/test` | Go 适配 | 在服务端读取保存的渠道凭据并发送测试消息，浏览器不接触原始配置 |
| `POST /api/v1/notifications/custom-message` | Go 适配 | 校验标题、图片地址和所选渠道，仅向当前已启用且已配置的渠道发送自定义消息 |
| `DELETE /api/v1/notifications/{id}` | Go 适配 | 删除通知渠道；React 端要求输入完整渠道名称确认 |
| `GET /api/v1/plugins` | Go 聚合 | 合并插件市场与已加载插件，只返回名称、版本、作者、简介和安装/运行状态；配置值、字段结构、脚本及页面 HTML 不返回浏览器 |
| `POST /api/v1/plugins/{id}/install` | Go 适配 | 仅允许安装当前用户可见的市场插件，并校验字符串插件 ID |
| `GET /api/v1/plugins/{id}` | Go 适配 | 将旧插件的嵌套字段定义转换为安全 schema；文本、路径、地址、模板和凭据只返回是否已配置，开关、下拉和多选可安全回显 |
| `PUT /api/v1/plugins/{id}` | Go 适配 | 在服务端合并插件配置，支持只写字段保留、替换或清除，并保留 schema 外的插件内部缓存；保存后由旧插件管理器重载配置 |
| `GET /api/v1/plugins/{id}/page` | Go 适配 | 将旧插件扩展页转换为有数量和长度限制的纯文本段落与表格；不返回 HTML、脚本、配置或通用插件方法，仅为已迁移的记录附加受限操作标识 |
| `DELETE /api/v1/plugins/{id}/page/records` | Go 适配 | 允许三类媒体插件按数字 ID 删除自身历史，或媒体库归档插件删除严格匹配命名规则的归档文件；插件、动作和参数均由两层白名单约束，归档删除还要求输入完整文件名确认 |
| `DELETE /api/v1/plugins/{id}` | Go 适配 | 卸载当前已安装插件；React 端要求输入完整插件名称确认 |

其他 `/api/*` 请求仍由 Go 网关透明转发给 Flask。React 首页、资源搜索、下载管理、订阅管理、探索页、站点管理、服务状态页、通知渠道页和插件管理页不再依赖旧 Jinja2 模板。订阅页已经支持新增和高级编辑，并复用现有站点、过滤规则、下载设置及保存目录。站点页和服务页支持当前筛选结果的限并发批量连接测试。站点 Cookie、API Key、User-Agent 和 RSS 地址，以及下载器、媒体服务器、通知渠道和插件的文本配置，均采用只写字段：浏览器只能看到是否已配置，不能读取旧值；编辑时由 Go 在服务端保留、替换或显式清除。通知编辑器和插件编辑器都根据旧模块提供的字段定义动态生成，不在 React 中重复硬编码。下载器编辑器已经支持维护媒体类型、二级分类、下载保存目录、容器访问目录和 qBittorrent 标签，并在服务端校验重复规则及字段长度。10 个现有插件扩展页均通过统一查看器展示服务端提取的纯文本和表格；旧插件提供的动态脚本、事件处理器、原始 HTML、内部配置与通用方法不会直接注入 React。三类媒体推荐/同步历史可通过数字 ID 白名单删除；媒体库归档只允许删除 `归档_YYYYMMDDHHMMSS.md` 格式的记录文件，并要求浏览器和服务端同时校验完整文件名确认，不会删除媒体库影片。

迁移期间，Python 端临时增加了 `/api/v1/library/mediaserver/latest`，用于以 Token 认证方式向 Go 提供最近入库数据；下载器接口补齐了目录隔离与下载目录字段，配置更新接口增加了 JSON 配置项兼容；通知接口补齐了全量渠道查询和状态字段解析；插件 REST 接口的 ID 类型修正为实际使用的字符串类名，并增加了仅供 Go 合并后保存配置的 `/api/v1/plugin/config`、读取扩展页结构化内容的 `/api/v1/plugin/page`，以及执行固定历史删除动作的 `/api/v1/plugin/page/action` 兼容入口。对应能力完全迁移到 Go 后应删除这些兼容代码。
