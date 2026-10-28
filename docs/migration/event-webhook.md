# Webhook 插件原生事件外发

2026-10-28。对应旧 `app/plugins/modules/webhook.py`：订阅事件并发送 `{"type":事件名,"data":事件数据}`。它与通知渠道的普通 Webhook、媒体服务器回调接收器分别配置、分别授权；这里不实现通知渠道的 Jinja 模板。

## 旧配置与协议

保留 `UserInstalledPlugins`、`plugin.Webhook` 的 `webhook_url`、`method` 及未知字段。Go 接管现代安装／卸载／配置／扩展页与旧 `/api/v1/plugin/*` 表单入口。完整地址、路径、查询参数和 URL 账户凭据只写不回显，留空保留、明确清除才移除。卸载保留配置和回执，重新安装保持关闭。独立原生消费者不会阻断 Prowlarr 等实际订阅搜索，未知搜索插件仍明确拒绝。

POST 发送 type/data JSON。GET 默认 `get_encoding=legacy`，保留原地址已有查询参数，追加 type 和重复的 data 参数，每个值为原 data 字典的一个键。这是旧 Requests 的实际编码行为，不是完整数据对象。明确选择 `get_encoding=json` 才把完整 JSON 作为一个 data 参数，会改变接收协议。依据旧插件、RequestUtils 与 [Requests 官方参数编码实现](https://raw.githubusercontent.com/psf/requests/main/src/requests/models.py)。

原 URL 账户凭据沿用 HTTP Basic，新增 `X-NASTool-Event-ID`。不跟随重定向，不将内容转发到 Location；HTTPS 验证证书，不复制旧 verify=False。明文 HTTP 单独确认。GET 长度由实际数据决定，接收端长度限制可能导致请求不确定；不能截断或改为 POST 冒充成功。

## 授权与发送回执

旧安装／地址不启动外发。纯 Go 模式下，当前管理员每次审核新配置指纹，分别确认原目标、可能包含媒体路径／站点链接／用户与私有字段的数据、旧 Python 及其他重复发送器已停止。`GoEventWebhook` 保存独立授权版本、完整原配置摘要和管理员范围；目标、方式、配置、管理员密码／API 密钥变化或卸载会阻止旧待办。新授权不接管旧回执，不扫描历史补发。

实际生产者在自己的事务中调用 `bus.PublishSignal`，同时保存 `GO_EVENT_WEBHOOK_OUTBOX`。领域回滚不能留下可发送项，队列静默忽略写入不能虚报原业务完整提交。未授权时不建外发队列、不增加接收端调用。不存在公开的任意“发布事件”接口。

纯 Go 后台每 10 秒读取至多 20 条 pending，共享持久执行锁。再次核对原配置和管理员范围，先保存 intent 再发一次请求。新的 HTTP/1 连接关闭复用、HTTP/2 流透明重放与重定向，无重试或改目标循环。2xx 记录 sent；非 2xx、失败或丢失响应为 uncertain。中断留下 intent，结果写回丢失也不重发。sent 只证明接收端应答，不能证明其业务执行完成或端到端 exactly-once。

React 插件页提供只读状态、本页最多 30 条与较早稳定编号查询，没有自动发送／重试。pending、intent、uncertain、blocked 可确认并注明原因后忽略，保留原事件、发送事实与操作人；不能撤回接收端操作，不删除媒体或操作下载器。设置／忽略应答丢失后先只读查回。

管理接口均要求当前管理员和纯 Go 模式：

- `GET /api/v1/event-webhooks`：仅原接收站点、授权及后台状态，不返回完整地址、凭据、私有摘要或事件内容。
- `GET /api/v1/event-webhooks/receipts?page=1`：本页最多 30 条完整回执。
- `GET /api/v1/event-webhooks/receipts/{64位编号}`：原稳定回执。
- `POST /api/v1/event-webhooks/receipts/{编号}/dismiss`：明确确认与原因。

配置原子更新额外核对实际修改行数及最终全部相关值，防止忽略已有行更新后插入重复授权、或后续触发器篡改先前已核对的值。

## 真实生产者与边界

| 事件 | 已接通的原生来源 |
| --- | --- |
| `transfer.finished` | 整理完成事务，每个媒体条目一项；原来源、文件、目标根、发布路径和媒体身份。原结构直接同步遵守原排除规则 |
| `download.add` | 手动下载、自定义 RSS、精确／模糊订阅及跨季联合接收；实际可用媒体、资源、下载器／路径／选集 |
| `subscribe.add`、`subscribe.finished` | 新增、洗版／模糊恢复和完成事务；原媒体、订阅编号及可用规则 |
| `sourcefile.deleted`、`libraryfile.deleted` | 已有明确删除事务中的结构化事实，不扩大后续删除权限 |
| `emby.webhook`、`jellyfin.webhook`、`plex.webhook` | 原接收授权、密钥和实际来源核对后的原始 JSON，包含暂停等不生成通知的合法事件 |

原始回调在原生外发授权尚开启时才进入私有队列，发送前再次核验当前管理员范围；公开通知回执仍仅规范通知字段。`GO_MEDIA_WEBHOOK_SIGNALS` 只保留原接收版本、编号、摘要与时间；显式上游编号在原版本内去重，没有编号不猜测。已接收回调不因后来启用外发而补发。旧版本仅有通知回执、尚无原始事件账本时，同一编号也不能被新外发或变化的动作接管；旧证明损坏拒绝接收。暂停／直播不生成虚构通知，也不查询服务器补全；JSON 大小、深度和重复键限制保持。

下载时机明确为原生接收／核实后的 `native_event.stage=committed`；旧 Python 在 Add 前发送，前置可变参数钩子没有被冒充为已移植。只保留实际可用的旧字段，不伪造缺少的媒体信息或设置。媒体回调保留原 JSON；其余业务增加原生观察上下文。订阅结束、下载接收、文件入库和外发成功仍是不同事实。

通用消费者接受旧枚举的 20 个事件名，合同测试不代表全部真实生产者已迁移。`download.fail`、`transfer.fail`、`subtitle.download`、`message.incoming`、`search.start`、`media.scrap.start`、`plugin.reload`、`douban.sync`、`autoseed.start`、`refresh.mediaserver`、`site.signin` 的独立事件与旧调用时机继续逐项接入。原生字幕／刮削／刷新执行器不证明旧钩子完整接通，SpeedLimiter、MediaSyncDel、MovieLike 不因收到回调而启动。

## 集中验证

先完成整个 Go 后台、实际生产者、React、兼容入口和用例，再集中测试。首轮发现参数重名导致编译失败；修正后的集中组进一步发现实际订阅搜索的插件白名单遗漏，以及混合模式错误码次序，已修复。失败轮不算通过。

- 修复后的模块集中组 94 个测试／子测试通过；包括实际订阅接收／结束、真实本地整理、三种媒体原始回调、未知响应、停用、权限、旧入口、协议与原子失败。
- 本批完整不缓存整库回归：33 个含测试包、3,033 个通过事件，无失败，HTTP 1442.511 秒、退出码 0。真实 Rclone／aria2 CLI 未跳过，仅子进程辅助入口跳过。日志 `full-final.jsonl` 在最后旧回执兼容补充前启动，不把它标为该补充后的全量。
- 六个完整核心包含整理的竞态：718 个通过事件、退出码 0；相关 HTTP 竞态 41 个通过事件。最后旧回执补充后，完整媒体服务器包 141、相关 HTTP 41 个竞态测试／子测试另外通过，无失败；日志 `media-compat-race.jsonl`、`http-compat-race.jsonl`。未为这处局部补充重复 24 分钟的无关远程用例。
- 前端完整 278 项测试通过，最后提示文字修正后类型／生产构建通过。主包 722.30 kB、gzip 201.35 kB，保持原 500 kB 警告阈值。最终 Go 静态检查、模块校验及本机／Linux amd64 无 CGO 构建通过。
- 隔离浏览器验证旧地址未启用、缺少独立确认被拒绝、每次重新确认、原编号查回、注明原因后忽略、刷新查回同一事实、GET 选择与停用。后台提供的两次相同虚构暂停回调只留下一个原事件、一次 POST；模拟端接收后断开响应，原回执 uncertain，不自动重发，忽略后原事实保留。
- 新授权切换旧 GET 后，仅未来新事件产生一次 GET，原查询参数保留，四个 data 参数值为 Event／EventId／Item／Private，204 回执 sent。重复旧回调不被新授权补发。整个模拟接收端最后计数 POST=1、GET=1、服务器补全=0，无意外目标。
- 最终 Go 构建重启同一临时数据库后，停用保留，两条原回执及事件标识不变；已停用情况下接收新的虚构回调不创建新的外发项。浏览器前段隐藏标签页多次控制超时，仅查回、不重复写入；更换可见标签页后完成交互，接收器本身由本机测试脚本准备，不把该准备动作算作接收管理 UI 的新增验收。

退出检查仍为 Python 360、Go 旧接口调用计数 8、代理／运行时引用 82、容器 Python 引用 45。代理计数新增一处为混合模式拒绝回归的测试配置，不是新增生产 Python 调用。Docker 于本次只读检查时仍未运行，未自行启动它。

真实第三方接收端、媒体服务器、NAS／跨挂载及生产镜像仍待验收。不删除其他保留功能仍使用的 Python，总体标准见 [Python 完全退出计划](python-exit.md)。
