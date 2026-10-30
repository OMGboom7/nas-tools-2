# 共享浏览器运行时

2026-10-30 实现批次：Go 管理独立的临时 Chromium／Chrome 进程，接通 .org 字幕页面、内置 `method=chrome` 表单搜索及 `note.chrome` 页面搜索、站点连接测试、订阅的浏览器详情解析与种子下载会话。使用 chromedp v0.14.2（其 Go 1.24 要求与当前项目匹配），不运行 Python 或 WebDriver。集中验证与隔离页面记录见下；真实提供方及候选镜像仍有明确验收边界。

## 当前实现范围

- 浏览器负责普通脚本和 DOM，所有页面 HTTP 请求均由 Go 拦截后执行并回填。Go 保持证书校验，关闭请求连接复用重放，沿用站点限流、代理与原字幕请求意图／回执；不把重定向或页面资源绕过原请求边界。
- 每次载入使用专属临时资料目录，同时最多两次浏览器载入，六十秒总预算。请求数、文档跳转、网络内容、DOM、表单值和脚本有上限。取消后关闭所属浏览器，清理仅限自己创建且目录身份未变的资料；不读取或关闭日常浏览器。
- 主文档保持原站点 origin。字幕页面仅访问原 .org origin；站点页面可加载不带账户头／Cookie 的 HTTPS 脚本和样式。图片、媒体、字体、WebSocket、服务工作线程旁路和跨站导航不参与本链路；浏览器自身网络使用不可达的本机代理，实际网络由 Go 执行。
- 表单读取原目录的 XPath 输入／按钮及预执行脚本，关键字按字面填入、只点击一次。等待动态表单及渲染结果后交给原解析器；不能把浏览器模板降级为普通 GET。
- 浏览器 Cookie 只用于原站点后续下载，绑定原站点记录、完整应用设置及三十分钟有效期；不写回 CONFIG_SITE、不进入公共 JSON／日志。私有模糊订阅文档可恢复这份原会话，旧的无会话文档序列化保持不变；到期或配置变化必须重新搜索。
- `NASTOOL_BROWSER_BINARY=auto` 自动查找本机安装，也可指定可执行文件。仅 Go-only 启用共享执行器，实际连接测试成功才说明该次载入可用；未安装／启动失败和 Cookie 失效分别报错。候选 Alpine 镜像加入 Chromium 和控制库许可证，继续以 UID 911 运行，保留浏览器沙箱及证书校验。

候选运行时基础版本改为 Alpine 3.24。按 [Alpine 官方支持表](https://alpinelinux.org/releases/)，旧 3.21 已仅维护 main，而 Chromium 位于 community；[3.24 官方构建定义](https://raw.githubusercontent.com/alpinelinux/aports/3.24-stable/community/chromium/APKBUILD) 列出 x86_64／aarch64 及原沙箱文件。构建时检查浏览器可执行文件和沙箱 setuid 文件，其他架构未验收；这不代替实际镜像构建／运行。

内置目录现有唯一 `method=chrome` 记录是 TNodeSpider，其既有原生专用 API 保留；新表单链路以同样目录协议的独立夹具验证。普通页面读取拒绝 POST，表单搜索允许同站 POST；需要另外业务动作的站点继续按明确调用者迁移，不能任意放开页面写入。

站点登录／Cookie 获取更新、各站签到与用户统计是后续独立业务；不会借搜索动作替用户登录或签到。需要人工验证时明确失败，不自动点击验证码，不忽略证书或关闭浏览器沙箱。真实站点、账户、NAS 与非 root 容器实际启动／健康／升级／回滚继续待验收，相关 Python 尚不删除。Docker 仍由用户自行启动。

## 集中验证

整模块实现后统一验证。完整核心四包竞态 83 项、相关 HTTP 竞态 33 项通过；包含真实隔离 Chrome、Go TLS 校验、一次表单动作、Cookie 删除／新增／HttpOnly、跨站及只读边界、来源变化、限额、丢失应答和取消。首轮发现 Chrome 以 Other 类型自动请求 favicon，现将其归入图片拦截并完整复验核心四包，原失败轮不计通过。

最后复核补强了不支持 JSON 指纹的 YAML 特殊值：生成指纹失败返回不可用，在进入渲染器前拒绝，不把两个失败配置都当作相同 SHA。核心四包和上述 HTTP 33 项已按补强后的源码再跑，静态检查及两个平台重新构建通过。整库执行器在该补强前已编译启动；其结果单列如下，没有在补强后再次运行全部用例。

不缓存整库 `go test -count=1 -timeout=60m -json ./...` 已完整结束：38 个含测试包、3,334 个测试／子测试通过，无失败，已取得退出码 0（HTTP 2673.675 秒）。真实 Rclone、aria2 和 Chrome 均未跳过；唯一测试 skip 为由父用例启动的 `TestJobGuardSubprocess` 辅助入口，另有五个没有测试的包。

前端最终 321 项通过，无失败或跳过，TypeScript／Vite 生产构建通过；主包 806.53 kB、gzip 222.97 kB，保留原有 500 kB 提示。登录页原先声称仍转发旧服务，已改为现有账户兼容说明。静态检查、依赖完整性、本机正常 Go 和 Linux amd64 无 CGO 构建通过。

隔离 React 使用正常 Go 二进制，连接两个只监听本机的虚构站点／下载器。站点登录标记由脚本生成，UI 显示“连接成功（原生浏览器）”；原目录的 XPath、预脚本及搜索按钮实际执行，关键词 `Movie & Query` 保持原样。模拟端收到一次表单查询、一次种子获取及一次下种，Cookie 由渲染传给原站点下载，未转发下载器；没有未知地址或真实账户操作。

下载原编号 `85236f30ea1b7fd0efc8775df1cafb61` 为 submitted／acknowledged，表示模拟下载器当时接收。相同正常 Go 程序重启后，UI 只读查回及数据库比对确认原编号、状态、时间及完整私有文档保持，未再次下种。原站点 Cookie、保留文件 SHA-256、旧历史 `9007199254740993`／BLOB `00FF01`／NULL 与未知配置整数保持，没有虚构入库。

集中日志为 `/private/tmp/nastool-browser-runtime-qa.NM3PIa`，页面／回执证据为 `/private/tmp/nastool-browser-runtime-browser.UAAl53`，保留 `connection.png` 和 `search-download.png`。重载后浏览器工具的截图／退出接口不可用，重启回执保留已读页面文本及独立数据库证据，不声称存在重启截图或已点退出。Go 进程和包含两个模拟端点的夹具进程已关闭并取得退出码 0，所有夹具文件／数据库保留；未操作真实文件、账户或已有容器。

本批最后一次只读 Docker 检查无法连接用户 Docker daemon，因此没有实际构建／运行候选镜像，也未自行启动 Docker。上述本机／静态验证不作为非 root 容器证据。

## 实施前审计（保留当时边界）

以下“当前”均指实施前基线；本批状态以页首和集中验证记录为准。

2026-10-30，OpenSubtitles 集中回归期间的只读审计。**本文件是后续实现范围，不是 Go 浏览器功能已完成的证明。** 当前 Go 模块没有浏览器控制库；无 Python 候选镜像也没有 Chromium。不能因保存了 chrome 开关、普通 HTTP 能访问某个测试页，或浏览器里成功操作 React，就认定旧浏览器执行器已迁移。

## 实际消费者

| 原实现 | 依赖的实际行为 | 当前边界与后续验收 |
| --- | --- | --- |
| `app/plugins/modules/opensubtitles.py` | Chrome 打开原 .org 页面、取渲染后的 DOM 与 Cookie，再由普通 HTTP 取 ZIP | 本批原生 HTML／REST 和补写链路见 [OpenSubtitles](opensubtitles.md)；旧浏览器载入与验证能力仍缺 |
| `app/indexer/client/_render_spider.py`、`builtin.py` | 原域名与 Cookie／UA／代理，部分模板填搜索框、提交按钮及预执行脚本 | Go `builtinindexer.Build` 明确拒绝 method=chrome；不是已支持而未配置。需分别核对页面载入、动作和解析，不把所有模板直接当 GET |
| `app/sites/siteconf.py` | render 模式载入详情，等待 JS 完成后按原站点规则解析 | HTTP 详情解析不证明浏览器渲染已接通；保留原站点／账号和详情路径绑定 |
| `app/sites/site_cookie.py`、`app/helper/sign_helper.py` | 站点登录、两步信息、原浏览器会话及 Cookie 更新 | 读取 DOM 不等于登录成功；独立核对凭据发送范围、成功凭据与未知应答，不借普通浏览器读取授权提交登录或更新账户 |
| `site_userinfo.py`、`siteuserinfo/_base.py`、`sites.py` | 部分站点浏览器页面、登录状态及用户统计 | Go 站点配置投影保留 note.chrome，不代表真实账号级 PT／统计证据已完成 |
| `site_signin.py`、`plugins/modules/autosignin.py` 及 `_autosignin/` 的 TJUPT、M-Team、BTSchool | 每站登录／签到流程、页面动作及服务结果 | 保留站点差异；不将状态码、打开页面或调度器启动当作签到成功，不以再次访问自动重放未知操作 |
| `app/helper/chrome_helper.py`、`web/main.py`、旧启动链路 | 查找浏览器、下载／绑定 WebDriver、进程退出和状态页 | 用 Go 管理浏览器进程和实际可用性；不能把“配置了二进制路径”当作启动、退出、镜像或健康检查通过 |

以上 Python 读取未执行真实站点操作。当前 Go 的浏览器拒绝行为由 `backend/internal/builtinindexer/request_test.go` 的明确断言覆盖；`site_native.go` 中 chrome 是旧字段投影。该边界需要正面替换，不能为了删除 Python 把保留业务标成成功。

## Go 替换路径

[Chrome 官方 Headless 说明](https://developer.chrome.com/docs/automation-and-testing/headless) 已采用统一的浏览器实现；可以由 Go 控制浏览器进程，而不继续使用 Python WebDriver。[chromedp 官方仓库](https://github.com/chromedp/chromedp) 提供 CDP、独立上下文及请求拦截能力，是待评估方案。选定前仍须核对具体版本的 Go 要求、CDP API、许可证、Linux／macOS 退出行为与 Chromium 分发方式；本次没有安装新依赖或启动该运行时。

实施顺序：

1. 先实现共用的原域名页面载入、实际渲染完成／失败、Cookie／UA／代理和取消／退出，配置变更不能沿用旧会话。每个账户使用隔离临时配置，不读取用户日常浏览器资料；运行时不动态下载 Python 或 WebDriver。
2. 将已确认的消费者接到这个原生边界：先 .org 原页面与 Cookie，再接需要浏览器的站点搜索／详情。模板动作及预执行脚本须按实际调用者审计，不能从页面内容扩大成任意本机操作。账号级证据仍用原站点来源。
3. 独立迁移登录／Cookie 更新、每站签到及统计，明确实际请求、持久意图、成功条件、未知结果和重复执行规则。浏览器可载入不证明这些业务已迁移。
4. 最后更新 Go-only 候选镜像的实际浏览器依赖，验证非 root 启动、证书／进程隔离、资源上限、停止／重启及全流程。Docker 仍由用户自行启动；本文件不把尚未构建或运行的镜像算作证据。

旧 helper 中有全局共享浏览器、隐藏窗口、关闭沙箱、忽略证书和自动点击验证控件等技术实现。它们不能机械复制成新的全局权限。正常页面渲染、原站点业务流程与需要用户参与的验证分别说明；出现证书或验证阻碍不记成功。尚无对应 Go 行为及验收前，相关 Python 继续保留。

## 旧调用计数的含义

当前 `postLegacy(` 共 8 处：`plugins.go` 中 3 处、`plugin_config.go` 中 2 处、`plugin_page.go` 中 2 处实际调用，加 `dashboard.go` 的共用函数定义。七个实际调用点集中在插件市场／运行列表、安装卸载、配置与扩展页面／动作。它们只代表显式调用，不覆盖通用反向代理、旧后台进程或上述浏览器消费者。

应在保留插件和真实页面动作逐项接通后删除这些旧分支；不能仅删函数、给整个插件目录返回空结果，或让 Go-only 返回 501，就宣布生产请求链路已退出 Python。完整退出仍以 [完成标准](python-exit.md) 为准。
