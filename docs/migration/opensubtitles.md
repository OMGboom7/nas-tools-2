# OpenSubtitles 原生检索、下载与文件回执

2026-10-30。本批将实际入库与单文件请求中的 OpenSubtitles 检索、字幕下载和原视频旁的补写接入 Go／React。原 ChineseSubFinder 接收任务、OpenSubtitles 下载文件及独立 Webhook 发送分别留痕；任何一方成功都不能代表其他一方完成。

## 原提供方与协议

旧 `app/plugins/modules/opensubtitles.py` 使用 opensubtitles.org 网页和 ZIP，不是 XML-RPC。Go 默认仍为 `org`，保持原 IMDb 优先、无 IMDb 电影名称／年份以及 TV 季集筛选；没有 IMDb 的 TV 不改成模糊电影查询。检索只采用实际识别字段及核验过的同一 TMDB 详情，不伪造 IMDb。

原网页只访问验证证书的 HTTPS `www.opensubtitles.org`／`dl.opensubtitles.org`，可使用独立配置的 Cookie，并保留合法网页 Cookie。遇验证页或无法识别的 HTML 时停止。**旧 ChromeHelper 的 undetected_chromedriver、自动处理 Cloudflare／浏览器验证没有在本批移植**；原网站需要该能力的真实环境仍未验收。不会关闭证书验证、自动解决 CAPTCHA，或静默改用 .com 账户。这是保留的迁移边界，不能称为原浏览器执行器已退出。

`com` 是明确选择的新提供方式，使用独立的 .com API Key、账户和 User-Agent；不会把原 .org 凭据自动搬过去。官方已公告旧 XML-RPC 退出，Go 因此没有新增该旧 API 依赖。依据 [官方 API 文档](https://ai.opensubtitles.com/api-docs)、[官方客户端示例](https://blog.opensubtitles.com/opensubtitles/how-to-build-a-movie-subtitle-finder-cli-with-python-and-the-opensubtitles-api) 与 [官方旧 API 退出公告](https://forum.opensubtitles.com/t/opensubtitles-org-api-final-shutdown-notice-for-non-vip-users/5045)：

- 登录为 `POST /api/v1/login`，仅接受官方返回的普通／VIP 固定 API 主机；电影使用 `imdb_id`，TV 使用 `parent_imdb_id` 和原季集条件，查询中文字幕。
- 每个 `POST /api/v1/download` 下载授权会消耗提供方配额，不能视为可重复的只读查询。只接受固定官方下载主机及 `/download/` 路径；取文件不携带 API Key、Bearer 或 .org Cookie。
- 登录会话只在进程内短期复用，正常缓存十五分钟，并受令牌到期限制；不保存令牌或临时签名链接到回执。登录至少间隔一秒。401／403／406／429 在原授权代次中持久阻止后续新调用，重新启用须再次明确审核授权。

没有访问真实 OpenSubtitles 账户或消耗真实配额；模拟服务不证明实际网站当前可用或一定能找到字幕。

## 来源、授权与不重发

保留 `UserInstalledPlugins`、`plugin.OpenSubtitles`、未知安装 ID、未知配置及大整数。卸载保留原配置。凭据只写，读取／聚合配置、错误和回执不回显 Cookie、用户名、密码、密钥、令牌和签名链接。安装和保存不发字幕请求；旧 enable 开关不能自动启动 Go。

独立原生授权绑定当前主管理员、完整原配置文本、安装状态和随机代次。分别确认媒体身份外传、只补缺失字幕，以及旧重复执行器实际停止。未来远程入库另需独立确认原账户／命名空间、独占写入和恢复副本费用；单文件远程请求仍需另外确认本次完整读取与字幕／持久副本写入，不借用未来入库授权。

完整入库成功事务记录原文件观察，即使当时未启用 Go 也冻结“未捕获”。之后启用、重复完成、刷新或重启都不补领该旧事件。已捕获请求绑定完整原任务／文件凭证和自己的 NFO／图片账本，等待自己的元数据完成；等待让出队列并由原来源唤醒，不占外部请求意图。

单文件继续使用原 `/api/v1/organization/subtitles`、旧 REST 和 `/do download_subtitle` 审核／提交。可仅用 OpenSubtitles，或同时使用 ChineseSubFinder；双消费者在同一排队事务冻结，缺失任何启用方的有效授权均拒绝整次提交，不悄悄跳过。较早没有 OpenSubtitles 的第一代私有定义及指纹不变。单文件不创建虚构入库任务、不改或新增整理历史。

领取和每个实际请求的意图事务都复核原来源、配置、授权、文件发布凭据及绑定。每个登录、检索、下载授权、归档及文件获取分别先持久保存唯一意图；标准 HTTP 传输关闭连接复用重放，有限重定向也逐项留意图。应答丢失、意图提交后崩溃、文件结果未知或 running 中断均不自动重跑。修复配置／重新授权不会接管旧 blocked／uncertain 项。

## 字幕文件与恢复凭据

下载内容最多 32 MiB；ZIP 最多 2,048 个成员、展开合计 64 MiB，单字幕最多 16 MiB。仅采用 .srt／.ass／.ssa，保留原编码字节，不解压归档路径到磁盘；绝对路径、穿越、符号链接、加密成员、无效 CRC、HTML 验证页或超限拒绝。

最多尝试三个不同候选，每个扩展名只用第一个。普通视频保持旧 `Movie.chi.zh-cn.srt` 命名；具有完整原盘证据且源扩展名为空时，保留旧 helper 的 `Disc.chi.srt` 结果。归档成员名不决定目标，写入只能是原媒体旁这三种固定名。

沿用已有库发布、目标占用、恢复与副本机制，使用专用标记关联原字幕项，NFO 后台不能领取这些工作。已有文件始终保留，不交换或覆盖。原视频与完整原盘继续核验，本地固定目录描述符及防覆盖发布、MinIO／真实 Rclone 原账户／完整摘要及已批准的持久副本分别使用原实现；远程没有通用原子发布。未知输出和副本留给原凭据的独立核对／处置，不自动清理。

持久结果区分 pending、running、completed、not_found、partial、blocked、uncertain、stopped；completed 只代表逐文件确实新增或保留。React 同时展示原提供方、原稳定编号、请求意图数、文件状态与更新时间。意图数不是字幕数量；只有尚未开始、无请求意图及发布工作的一项可注明原因后停止，不能把本地停止冒充远端取消。

## 接口与当前验证

- 原生插件 `/api/v1/plugins/OpenSubtitles` 的配置／状态／安装／卸载／只读页，以及对应旧 form 入口由 Go 接管；混合模式修改拒绝，未知动作拒绝，不回退 Python。
- `GET /api/v1/open-subtitles` 核对当前授权；`GET /requests?page=1` 每页三十条，`GET /requests/{id}` 查询原编号；`POST /requests/{id}/stop` 只能明确停止尚未开始项。
- 混合安装列表包含未迁移插件时，旧聚合配置接口明确拒绝转交整份原生字幕凭据；各个原生页面仍可用。

本批集中验证已通过完整核心五包竞态 663 项、相关 HTTP 竞态 64 项（70.457 秒）、前端 321 项和生产构建、静态检查／依赖完整性校验、本机与 Linux amd64 无 CGO 构建。覆盖本地四模式及整盘、双消费者、来源／配置变化、事务内替换与静默忽略、真实 SQLite 关闭重开、拒绝后的同代次停发、MinIO 与真实 Rclone 的单文件／入库写入及元数据先后顺序。早期失败轮用于定位并修正链路问题，不计为通过证据。

最终不缓存 `go test -count=1 -timeout=60m -json ./...` 的 37 个含测试包、3,311 个测试／子测试通过，无失败，已取得退出码 0（HTTP 1650.824 秒）。唯一 skip 为需由父用例启动的 `TestJobGuardSubprocess` 辅助入口；真实 Rclone／aria2 未跳过。首轮整库发现两项目录测试仍按旧 OpenSubtitles 描述／未迁移状态断言，修正这两项预期并独立复验后，再完整跑完上述最终回归；生产源码没有为迎合旧目录预期改回去。

前端完整 321 项无失败或跳过；TypeScript／Vite 通过，主包 806.53 kB、gzip 222.99 kB，现有 500 kB 提示阈值保留。最终编译、竞态与浏览器运行使用相同生产 Go 源码，之后仅更新目录测试及迁移记录。

浏览器使用当前最终 Go 业务代码和 React，实际完成一次字幕 ZIP 下载：原 .srt 字节保持，只新增缺失 .ass，分别为 preserved／created；请求 `d76da70e781532167be0147d90a4d5f674604d0609e6a4d11819d6148e674a2d` 为 completed。第二次由模拟服务接收下载授权后断开，原请求 `adf75dabdc69febd6d85c063358e2f09f5aab83644d42f25cdd7416f5115e589` 保留 uncertain，没有写入第二份影片字幕。

正常 Go 二进制重启同一临时数据库后，原编号、状态、时间与请求意图数保持；模拟服务仍只有两次下载授权、一次归档获取和一次登录，无未知目标。五份原文件 SHA-256、唯一旧历史 `9007199254740993`／`00FF01`／NULL、未知配置整数及零虚构入库均保持。网页退出，原 enable 与 Go 授权关闭，普通 Go、夹具服务和页面均已关闭；没有卸载或删除原配置／回执。

集中日志 `/private/tmp/nastool-opensubtitles-qa.CIx3Vv`，最终全量为 `all-verified.jsonl`；浏览器证据／截图 `/private/tmp/nastool-opensubtitles-browser.9MfhUX`。夹具启动器只在进程内信任并连接本机模拟 HTTPS 端点，普通 Go 重启时出站代理也固定本机，未改系统证书或生产入口。没有把该启动器当作生产部署证据。

退出指标目前为 360 个 Python 文件、8 处旧接口调用、87 处旧代理／运行时引用、45 处容器 Python 引用；本批增加的一处代理引用来自混合模式拒绝测试，没有新增生产 Python 依赖。整体仍粗估 50–60%，不按测试或文件数提高。旧浏览器验证能力、其他保留业务、真实服务／NAS、存量升级／回滚、实际无 Python 候选镜像构建／运行／健康检查仍未全部完成；Python 尚不删除，Docker 由用户自行启动。
