# 站点用户统计 Go／React 实现与验收

2026-11-03：Go 统计采集、兼容统计表、后台执行与 React 面板已接通，并完成集中测试和隔离页面验收。全项目 Python 退出尚未完成，原 Python 生产入口暂保留。

## 实现范围

原实现为 `app/sites/site_userinfo.py` 及 `app/sites/siteuserinfo/`。`backend/internal/sitestats` 保留识别优先级及 12 类协议：Gazelle、Mteam、NexusRabbit、Discuz、Unit3d、NexusProject、SmallHorse、Ipt、TorrentLeech、FileList、TNode、NexusPhp。覆盖账户资料、上传／下载、积分、做种明细和分页；M-Team 包含最后一页，Nexus 无 ID 详情页保留内嵌做种表和汇总回退。只有汇总时保留原做种明细，不将未知明细清空。

同轮抓取共享 Cookie；接入站点代理、UA、限流、HTML 编码及现有 Go 浏览器。浏览器取得／删除的 Cookie 同步到 HTTP，TNode／Rabbit JSON 和图标保留原始响应与 CSRF 头。页面链接和重定向限定同源，M-Team API Key 只发送至原站点推导的 API 地址。相对分页按当前页面解析；循环、缺页、登录失效和错误表格不能覆盖旧统计。

成功站点一次事务发布 `SITE_USER_INFO_STATS`、`SITE_USER_SEEDING_INFO`、`SITE_STATISTICS_HISTORY`、favicon 及通知事件，事务内核对原站点记录。保留 `EXT_INFO`、未知列、大整数、BLOB、NULL。新 React 接口的字节量使用十进制字符串；旧兼容 API 保留数字字段。

手动刷新有稳定请求编号，并发上限 10，进程锁覆盖执行。停止标记持久化，其他进程也须停止后续请求和发布；遗留活动回执在锁内转为未知，不自动重做。后台默认关闭，显式 Go 接管后才执行。保留原数字小时／小数小时、HH:mm、时间窗口、五字段 cron、时区及一分钟首次延迟，避免误用 RSS 分钟单位。

原交互渠道 `/sta` 共享验证和传输日志，可独立于签到启用。旧授权／旧时间不能扩大范围，被拒绝的统计命令不会堵塞后续签到消息。Nexus 未读内容默认不读取，界面明确提示可能标记已读；手动请求和后台授权分别绑定该选择。开启时保留读取前数量、分页和内容；`site_message`、`ptrefresh_date_message` 复用原开关及通知链。

React 展示统计表、每日历史、做种、刷新回执、后台范围及停止。Go 原生实现 GET `site/statistics` 和 POST `activity`／`history`／`seedinfo`；旧 GET 保留 API Key，旧只读 POST 保留普通登录权限，刷新／接管要求主管理员。

## 集中验证

- 统计核心竞态 44 项、签到核心 48 项、消息总线 58 项，无失败／跳过。包括已安装 Chrome 与隔离本机 TNode 的真实 Cookie → 原始 JSON／CSRF 接口链，未将模拟 Renderer 当作真实浏览器证据。
- 相关 HTTP 竞态 33 项，无失败／跳过：刷新、失败保留、原编号、重启、跨实例停止、原定时格式、普通用户只读、独立 `/sta` 回调与传输日志。
- 前端完整 341 项及生产构建通过；Go 静态检查、本机／Linux 无 CGO 构建通过。本批未重跑后端全库。
- 固定样本覆盖 12 类协议、分页／编码、精度、事务失败、未知列、Cookie 删除、未读数量、嵌套布局、错误／截断表格和详情页回退。

主日志目录 `/private/tmp/nastool-site-statistics.sXE7md`：最终核心 `core-final-accepted.jsonl`、HTTP `http-release.jsonl`、前端 `frontend-final.log`，另有构建／静态检查记录和二进制。

真实页面证据为 `/private/tmp/nastool-stats-browser-final.XpfSEa/browser-proof.json` 和 `browser-after-restart.jpg`：正常 Go／React 请求本机模拟 Nexus 共 8 次，包含两页做种和一次虚构消息读取。2 个种子共 3.50 GiB，保留两天历史，消息与汇总各通知一次本机 Webhook。原站点、未知大整数／BLOB／NULL 和 keep 文件保持；临时后台范围已停用。正常 Go 重启前后任务完整文档逐字相同，无重复请求／通知。已退出并关闭标签页，两次 Go 及监督进程均正常退出。

早期验收不计通过：系统网络规则使大脚本仅返回首个 512 字节，该空白页面未执行统计，环境已关闭。后续正常临时环境发现相对分页错误，保留失败回执和旧统计；修正并补样本后，在新临时库完成上述验收。最终页面使用拒绝外部转发的本机 HTTP／HTTPS 代理，不称作操作系统级网络隔离。早期错误表头及路径形式断言失败日志同样保留。

## 全域剩余工作

真实站点／账户、存量升级／回滚、最终全库及容器运行仍待验收。用户等级排行规则和派生字段属于独立业务，本批不宣称已迁移；原 HTML／WebAction 生产调用者须随全域切换移除。继续签到剩余辅助分支及其他保留插件。

退出指标仍为 360 个 Python 文件、8 处 Go 旧接口调用、87 处旧代理／运行时引用、45 处容器 Python 引用，整体粗估 50–60%。本批只读 Docker 检查仍无法连接 daemon；未启动 Docker 或操作已有容器。
