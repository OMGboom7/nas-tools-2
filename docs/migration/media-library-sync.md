# 媒体库展示快照：手动同步

2026-10-16：保留旧媒体库同步能力，先迁移手动同步闭环。Go-only 模式的管理员可在 React「媒体库」页读取影视库列表、选择范围、分别确认旧同步任务已停止及替换展示登记簿，完整读取 Emby / Jellyfin / Plex 后原子发布到原 `media.db`。此操作只更新登记簿，不修改媒体服务器、不搬动或删除媒体文件。

## 真实数据与原地兼容

- Emby/Jellyfin 使用原 `/Users/{id}/Views` 目录及递归、分页影视查询，获取电影/剧集身份、年份、可用的 TMDB/IMDB、原名/路径和非虚拟剧集；明确配置用户名不存在或禁用时不切换其他用户，未配置时选启用的管理员。
- Plex 支持 JSON/XML 库、电影/剧集与 `allLeaves` 剧集，读取可用的 GUID；当前不额外请求原名/媒体文件路径。原本就没有 GUID 的条目不会凭标题伪造 TMDB 身份。
- 分页必须给出稳定总数，拒绝重复 ID、缺页、总数变化、错误信封、无效类型/剧集编号、请求失败及取消。Emby/Jellyfin 必须返回明确的 Items 数组与 TotalRecordCount；Plex 内容页必须提供 totalSize。旧版本/代理不提供这些证据时明确失败，不把未知内容当空库。单库/单剧原始条目上限 100000，所有所选库影视总数上限 100000，展开剧集总数上限 1000000；超限整次失败。
- 剧集保存原 `season_num` / `episode_num` JSON；合并集号展开并按季/集去重，保留第 0 季及第 0 集。不用“有一集”推断整季齐全。
- 首次打开已有数据库先保留一致性备份 `media.db.pre-go.bak`。兼容旧 `MEDIASYNC_ITEMS`（含历史拼写 `ORGIN_TITLE`）和 `MEDIASYNC_STATISTICS`，新增 `GO_MEDIASYNC_SNAPSHOTS` 记录配置摘要、所选库、操作人、代次及完成时间。
- 获取期间不清旧表，完整获取后一个 SQLite 事务替换所选服务器类型的全部条目、旧统计及 Go 快照元数据；任一步失败回滚，其他服务器类型不受影响。未选择的旧库条目会从该类型的展示登记簿移除，所以禁止空选择。用户确实选中的空库在正向完整响应后允许发布空快照。
- 整次请求持有跨进程锁，无租约超时解锁；读取后再次检查当前管理员权限及服务器配置。服务端最长 10 分钟，取消不继续发布。响应丢失后界面只读刷新状态，不自动再发同步。

## API 与使用边界

- `GET /api/v1/library/sync?page=1`：仅查本地 Go 快照，每页 100 项，标识 displayOnly 与当前配置是否匹配，不访问媒体服务器。首次 Go 同步前不把旧 Python 无来源登记簿冒充 Go 已核验快照，也不改旧数据。
- `GET /api/v1/library/sync/libraries`：只读查询当前影视库并返回配置指纹，API key/token 仅放请求头，不进入 URL/响应。
- `POST /api/v1/library/sync`：明确 `libraries`、`fingerprint`、`confirm=true`、`legacyStopped=true` 后完整同步，纯 Go 模式且管理员才可执行；混合运行返回 501，不代理给旧服务。
- 快照配置摘要不匹配时仍明确标识旧配置，不能显示成当前服务器实时状态。同一配置的快照也只代表上次读取时刻，不是远端事务快照；远端数量未变但内容替换等并发变化无法由本地分页彻底排除。
- 手动接口不更改旧 `SyncLibrary` 设置、不启动定时计划；所选范围保存在新快照元数据。后续同日已接入 [独立授权的 Go 定时同步](media-library-schedule.md)，与手动接口共用采集/发布底座。兼容旧页面 `start_mediasync` 动作、旧媒体卡片“已存在”标记和同步进度轮询仍需后续迁移。
- **下载/订阅的实时 `Inventory` 查重保持原实现，绝不使用这份缓存证明资源缺失或决定下种。**

## 验证与后续

本批验证范围为临时 SQLite、隔离模拟媒体服务器、接口权限/配置变更/跨进程互斥、前端请求契约与构建。真实服务器兼容性、大型媒体库、浏览器操作和生产候选镜像尚须验收。本模块不是整个媒体服务/后台任务全部替代，不能据此删除 Python。

本批集中验证已通过：`go test ./...`、`go vet ./...`、CGO 禁用的 Linux amd64 构建；mediaserver / mediasync / httpserver 的 Snapshot、MediaSync、Inventory、Coverage、Dashboard 相关 `-race` 测试；前端 92 项测试与 TypeScript/Vite 构建。退出检查仍为 360 个 Python 文件、13 处 Go 显式旧接口调用、75 处旧代理/运行时引用、45 处容器 Python 引用，最终退出门禁尚未达标。

定时同步已在后续批次复用同一采集/原子发布逻辑，处理重启、配置变化与授权失效；下一批继续旧展示标记/进度接口。全局进度继续粗估 50–60%，不按本批新增接口数量提高完成率。

协议对照：仓库 `app/mediaserver/client/{emby,jellyfin,plex}.py` 与 [Plex 官方 API](https://developer.plex.tv/pms/)。
