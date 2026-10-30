# 共享浏览器运行时：剩余调用审计

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
