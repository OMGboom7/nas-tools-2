# 播放联动下载器限速

本模块将原 `SpeedLimiter` 的实际媒体会话读取、qBittorrent／Transmission **全局**速度控制、周期检查与播放回调接入 Go，React 提供独立审核、启停、原编号回执和不确定设置处置。原 `plugin.SpeedLimiter` 数据保留；安装、配置保存、旧表单修改和重装都保持停止，不能自动接管旧执行器。

## 原配置与执行规则

`download_limit`、`upload_limit` 为播放速度，`download_unlimit`、`upload_unlimit` 为无非豁免播放时的速度。0 表示不限速。`bandwidth` 为 Mbps，正值启用智能上行分配，沿用旧 `int(float(value))` 转换；读取完整当前会话后，以总带宽减去实际播放码率，剩余上传速度按原所选下载器顺序的 `allocation_ratio` 分配，每个实际启用下载器至少 10 KB/s。禁用／已不存在的下载器不执行；仍保留其原比例位置，剩余下载器按自己的权重分配。比例格式或数量错误沿用旧行为均分，审核显示实际权重。空闲组不按比例分配。

`ipv4`／`ipv6` 是**不限速来源**，不是回调来源授权。空值分别沿用 `0.0.0.0/0`／`::/0`；两族全部放行时不启用。接受明确 IP、CIDR 及当前会话中的 IP:端口；未知客户端地址不会当作可限速或无人播放。配置不接受含主机位的网段、域名、IPv6 zone 或映射前缀。回调来源仍由独立媒体接收器核对。

`interval` 留空 300 秒，范围 1–86400。新授权只创建今后的当前时间桶，不回补停止期间的历史桶。回调只记录需检查的触发器：Emby `playback.start/stop`、Jellyfin `PlaybackStart/Stop`、Plex `media.play/stop`；Plex 保留三秒等待。原回调身份去重、业务事件和触发器在同一数据库事务提交；新授权不能领用旧回调。

## 当前会话与协议差异

Emby 从原服务器 `/emby/Sessions` 读取 `NowPlayingItem.Bitrate`，Jellyfin 从 `/Sessions` 累加当前影片全部 `MediaStreams[].BitRate`。只计非暂停视频；音频／照片和非播放会话不计。Plex 从原 `/status/sessions` 接受完整 JSON／XML，核对 size 与实际条目数，只计 playing／buffering 视频。Plex 的 `Media[].bitrate` 按 Kbit/s 转为 bit/s，纠正旧代码未转换单位的问题。缺少播放状态、重复关键字段、错误地址、截断或请求失败均不能作为空闲证明。

视频码率未知仍被识别为“有人播放”：固定限速可以执行，智能分配会阻止，不编造码率或套用空闲组。暂停不参与播放限速，保留旧行为。不会信任回调携带的连接地址、账户、人数或码率。

qBittorrent 使用 transfer 的当前全局上传／下载 getters，冻结普通／备用模式，KB/s 按 1024 转换为 B/s；上传、下载各自为单次请求，分别记录结果，支持原当前备用模式但不切换模式。设置限制为旧服务也能接受的有符号 32 位 bytes/s。不会改每个种子的限速、分享率或停止任务。

Transmission 支持旧 session-get/set 与新 JSON-RPC 2.0 session_get/set，两代字段分别映射；保留原 RPC 数值单位及合法小数基线，不将观察值取整后冒充相同。只修改普通速度的四个字段，不改变备用或定时设置。备用模式当前启用时会拒绝审核／执行，避免普通速度被备用设置覆盖而仍声称有效。CSRF／协议协商只发生在只读 session-get；写请求不重新协商、不重放。

实现依据：[qBittorrent WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0))、[qBittorrent 当前模式实现](https://github.com/qbittorrent/qBittorrent/blob/release-5.0.0/src/base/bittorrent/sessionimpl.cpp)、[Transmission RPC](https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md) 和 [Transmission 4.0.6 RPC](https://github.com/transmission/transmission/blob/4.0.6/docs/rpc-spec.md)。

## 独立授权与稳定回执

管理员先核对保存的原配置，再明确允许读取原明文连接；Plex 账户 fallback 还需单独允许 plex.tv 登录及可选的明文原服务器连接。只读审核绑定管理员权限、配置、原下载器、媒体身份和当前完整全局速度，有效十分钟。启用前再次核对媒体身份和原速度，分别确认未来全局调整、0 的含义、旧执行器停止及所选状态通知。

旧 Plex 用户名／密码／指定服务器模式复用原生账户连接器，认证只在明确授权的同一世代内尝试一次，成功和失败只保留在内存；不静默尝试其他账户、认证方式或服务器。凭据、连接 URL、私有配置和指纹不出现在公开执行回执。

后台和管理操作使用同一持久进程锁。每次检查冻结独立稳定任务编号、完整前后值及逐请求计划；每个真正写请求前先提交 intent，再发一次。只有应答成功且完整回读与原目标值一致才记录 confirmed。单个 qB 请求结果不明会阻止该下载器后续维度，其他原下载器仍可分别执行；不会声称整组成功。

外部改值、模式变化、下载器／媒体配置或身份变化、原管理员变更，均阻止待执行设置。网络不确定和进程中断保留 uncertain／intent，不自动重发，也阻止同下载器的新自动任务。进程重启不会续跑旧计划。

React 支持分页和完整稳定编号查询。管理员可只读核对原下载器是否**当前**等于原目标完整快照，记为 verified_current；这不证明设置者或旧请求是否送达。不能证明相同则仍保留不确定。明确忽略需填写原因，同时停止当前自动限速，已确认／核对的事实保留；没有远端回滚。重新启用需要新的完整审核。停止插件保留下载器当前速度，不将“停止”冒充恢复原值。

状态通知在整组确认后，与播放状态和最终回执同事务进入持久业务队列，按旧 **custom_message** 开关选接收渠道。固定限速只在播放／空闲状态切换时通知；智能模式的播放回调还可通知新分配。通知发送与速率设置独立，通知失败不重做下载器操作。

## 验收边界

本模块完成后仍不等于 Python 已退出；MediaSyncDel、MovieLike、其他事件生产时机、账户级 PT、真实 NAS／下载器和存量升级、候选镜像实际运行继续在整体目标范围内。协议测试使用隔离虚构服务器，不以此声称已连接用户的真实服务。

## 集中验证

模块后台、React、旧入口和整组用例写完后集中运行。首轮前端通过；Go 组发现私有触发世代未随任务定义序列化，完整性核对拦截计划，已增加明确的内部世代字段。测试夹具随后区分了媒体接收器自身已授权的查询与限速副作用，并保留原订阅搜索夹具。失败轮不计通过。

不缓存整库回归 34 个含测试包、3,120 个测试／子测试通过、无失败，HTTP 包 1417.044 秒、退出码 0；真实 Rclone／aria2 CLI 已显式提供且未跳过，唯一测试跳过为专用子进程 helper。该全量在最后比例空白兼容修正与额外权限／途中停用／审核期限场景补充前启动；不能将它称作补充后再次整库运行的结果。

最终冻结代码的五个核心包竞态 413 项、关联 HTTP 竞态 66 项通过；覆盖三种媒体服务、完整会话失败／未知码率、精确单位和两代 RPC、实际回调与定时、智能分配、单个请求丢失与另一下载器独立完成、原编号核对、明确忽略、旧配置／未知大整数、审核期限、管理员权限以及请求途中停止。最后比例空白兼容补充之后已重新运行这些完整相关组。

前端完整 288 项测试、TypeScript／生产构建通过，主包 740.73 kB（gzip 206.39 kB），原 500 kB 构建提示仍保留。最终静态检查、模块校验、本机 ARM64 构建及 CGO 关闭的 Linux amd64 静态构建通过。日志和产物位于 `/private/tmp/nastool-speed-limiter-qa.IXc6YI`。

## 隔离浏览器与真实进程重启

采用 `/private/tmp/nastool-speed-limiter-browser.QALmsj` 内新建的配置／数据库、虚构管理员、127.0.0.1 媒体服务和两个虚构下载器，运行正常 Go 入口及本批 React 构建，不启动 Python，不接触真实媒体、下载器、账户或通知渠道。

浏览器验证原配置默认停止、连接读取与未来全局调整独立确认，没有预选权限；启用前设置计数为零。独立媒体接收授权由临时脚本设置，未借此声称重新验收了接收器授权页面。重复播放回调只产生一个稳定任务，实际 qB 上传请求被虚构服务接收后丢失应答；qB 下载维度未发送，Transmission 普通限速单次完成，原任务状态为 partial。

首任务编号 `67e0dc3bbaf45b413839b973e20562d1ff254653c1897a2e5feb63b80b544528`。页面只读核对原上传值后，事实为 verified_current／blocked／confirmed；不新增任何速度请求。填写原因并明确忽略后，原未结束项变 ignored，当前后台停止，已核对／已完成事实保留。刷新后按原完整编号查回同一回执。

随后使用临时 API 重新审核并创建第二份虚构下载设置不确定回执 `9b1f90af6547a5bbabd5f215f12ebde9e2864f45b87cde7ca20c30559d8efc39`，真实停止并重新启动 Go 进程。旧不确定状态和编号保留；新的播放事件被阻止，没有重发。浏览器查回该原编号、明确忽略并核对后台已停止。最终总数为 qB 设置 2 次、Transmission 设置 1 次；没有额外或未知写操作。概览页另有 3 次未提供响应的只读影视统计／用户查询，均已保留在证据中，不计作限速读取或声称全站展示验收通过。

最终冻结二进制再次以同一临时数据库启动，健康状态正常，两份 ignored 回执及 verified_current／ignored／confirmed 原事实保持，接收和限速授权均关闭。测试账户已退出，页面及全部本批服务已关闭；临时 token 文件已移除。截图为 `review-consents.png`、`partial-original.png`、`verified-current.png`、`ignored-stopped.png`、`restarted-original-uncertain.png` 和 `final-stopped-status.png`；保存的 JSON 证据核对了实际请求计数。
