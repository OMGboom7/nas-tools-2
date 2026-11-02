# 站点签到：实施前审计

2026-11-02，基线 `288a45f`。这是完整模块的实施范围，尚未实现 Go 签到执行器，也未运行签到测试或真实站点动作。先完成整条调用链，再集中验证；不以通用 GET 或少数简单站点替代整个模块。

## 原消费者与数据

实际插件为 `app/plugins/modules/autosignin.py`：保留 `enabled`、`cron`、`sign_sites`、`special_sites`、`retry_keyword`、`queue_cnt`、`notify`、`onlyonce`、`clean`、`auto_cf`、`missed_detection`、`missed_schedule` 及未知配置字段。原插件加载 `_autosignin` 的 21 个站点处理器；通用分支还有 1ptba 首页差异。

原 `/pts` 命令产生 `site.signin`。当前 Python 同时在漏签检查与实际签到上注册该事件；Go 需要统一接收、调度和去重，不能复制出两个相互独立的签到执行器。原通知进入系统消息中心，并按 `site_signin` 渠道开关发送；自动签到插件另有汇总通知。需接通实际消费者及状态，而不只添加事件名称。

旧持久日历史通过 `PLUGINHISTORY` 模型，以插件名 `AutoSignIn` 和本地日期为键，值中保留 `sign`／`retry`。展示结果 `_last_run_results_list` 则是进程内列表，并非旧日历史的完整逐请求账本。三个答案缓存是原配置目录下 `temp/signin/tjupt.json`、`52pt.json`、`chdbits.json`；不能为迁移重建或删除这些文件。时区来自 `TZ`，日期去重、U2 时间限制、跨月和年底漏签计算要按实际日历处理。

旧 `app/sites/site_signin.py` 还引用未在当前仓库出现的 `app.sites.sitesignin` 包，并向当前不接受 `signin` 参数的 `Sites.get_sites` 传该参数。这份旧文件不能直接证明存在另一条可用的生产签到链；通用选择器仍须以实际 AutoSignIn 分支和 `SiteConf._SITE_CHECKIN_XPATH` 核对。

## 21 个站点处理器

下表来自原处理器代码，不表示已与今天的真实网站联调。目标应从原配置的站点和已核对的处理器映射构建；不能通过模糊包含域名把 Cookie 发往另一个来源。

| 处理器 | 原协议与必须保留的行为 |
| --- | --- |
| CarPT、HDFans、HDTime | `attendance.php` GET；分别核对成功和重复签到文本 |
| HDCity | `/sign` GET，魅力／已签到文本及失效判断 |
| HaiDan | `signin.php` GET，已经打卡标记 |
| Hhanclub | `attendance.php` GET、`rule-tips` 与今日排名证据 |
| HDArea | `sign_in.php` POST，原 `action=sign_in` |
| Hares | 首页检查后 `attendance.php?action=sign`，JSON `code`／`msg` |
| PTerClub | `attendance-ajax.php`，JSON `status`／`message` |
| HDChina | 原 `hdchina` Cookie、首页新会话、meta `x-csrf`、`plugin_sign-in.php?cmd=signin` POST |
| HDUpt | 首页已签标记、`added.php?action=qiandao` POST、数值结果 |
| TTG | 首页 `signed_timestamp`／`signed_token`，`signed.php` POST |
| ZhuQue | 首页 `x-csrf-token`，原 JSON 技能请求；登录有效与技能成功须分别报告 |
| BTSchool | 普通 HTTP 与浏览器两分支，首页和 `index.php?action=addbonus`，原魔力／每日签到标记 |
| M-Team | 浏览器访问和可选原保存账户登录；复用独立登录能力处理输入／会话，邮箱挑战不冒充签到完成 |
| HDSky | 首页预检，创建图片码、取图、原 OCR、`showup.php` 提交；区分日期已签和图片码失效 |
| OpenCD | 首页、签到参数页、图片与 `imagehash`、原 OCR、`plugin_sign-in.php?cmd=signin` POST |
| U2 | 9 点后的时间限制；原多语言已签标记、隐藏字段和四个提交选项、`showup.php?action=show` |
| TJUPT | 答案图片／选项、原本地缓存、10×10 灰度图像哈希、豆瓣海报相似度及 Google Lens 后备、原提交表单 |
| 52PT、CHDBits | 原 `bakatest.php` 多选题／字段、缓存、可选原 OpenAI 兼容接口和随机分支、成功后答案缓存 |

合计 21 个处理器。通用浏览器路径还需原 11 个签到 XPath、动态页面等待、一次动作、当前来源会话和实际结果检查；浏览器防护仍交给用户，不复制旧自动点击防护控件。

## Go 实施与集中验收

1. 实现原站点与辅助服务的完整客户端：原 Cookie／UA／代理／限流、响应会话、HTML／JSON／图片解析和各站分支。辅助 OCR、豆瓣／图片、Lens、AI 的接收方和实际数据须可核对；保持原自建 HTTP／HTTPS 配置，拒绝／不明应答不自动换提供方。原 API 协议及模型配置需查原代码与提供方主文档，不静默换模型。
2. 实现单站与批量的持久意图／结果、逐请求与浏览器动作回执、单次输入、跨进程互斥、取消与重启。区分登录有效、原本已签、本次确认成功、失败及未知；JSON 非零码或仅点击按钮不能通算成功。
3. 接通 AutoSignIn 的原配置、接管状态、cron／漏签／特殊站点／一次运行、事件和消息。旧 `enabled`／`onlyonce` 不直接在候选进程启动新执行；不把旧日历史直接当成 Go 已核实成功。原 `get_sites(siteids=[])` 会扩大到全站，新的明确空选择不得变成全站执行。未知动作不参与自动重试；保留可解释的原始历史和本次状态。
4. 接通 React 的设置、目标与辅助接收方核对、运行／停止、挑战、结果／历史、刷新和服务重启。保留旧参数及未知值，旧缓存写入与本次成功证据绑定。自动 CF 优选是另一个真实外部消费者，不能因旧配置存在便运行尚未迁移的 Python 优选器。
5. 整模块实现后集中验证：全部处理器、普通及真实隔离 Chrome、缓存／原数据保留、脚本与令牌、失败／未知／撤销、跨进程／重启、日期边界、调度／消息、UI 原回执和候选运行时。优先进行覆盖相关消费者的验证；不为每个微小实现步骤重复整库长回归。

当前仅完成源码审计与上述范围记录。站点登录／Cookie 的 Go 实现及授权后 UI 验收另见 [站点登录](site-login.md)；签到或用户统计尚不能因此认定完成，Python 保留。
