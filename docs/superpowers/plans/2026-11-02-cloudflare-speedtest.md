# Cloudflare IP 优选 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan in the existing branch. The user already authorized continued migration, commit and push; do not ask again. Complete the module before running its test suite.

**Goal:** 将原 CF 优选插件及签到依赖完整迁移到 Go／React，保留原配置、Hosts 和可用安装。

**Architecture:** Go 管理原 Go CLI，安装、执行和纯 Hosts 规划放在独立 `cloudflare` 包。持久接管／意图／回执管理运行生命周期，HTTP／React 使用原插件入口，签到通过原批次编号提交一次触发。

**Tech Stack:** Go 标准库、SQLite、现有配置／任务锁／消息总线、React／TypeScript。

**Spec:** `docs/migration/cloudflare-speedtest.md`

## Global Constraints

- 生产请求不访问 Python；原插件数据和未知 JSON 值保留。
- 同一模块整体接通后集中测试；过程必要编译不算模块验收。
- 每一批实际修改提交并推送现有 `codex/react-go-refactor`。
- 不执行真实公网测速，不改本机系统 Hosts；验收仅使用自建临时数据。

## Review Focus

- 新版资产名称变化、离线环境及重装失败仍保留原可用安装。
- 原参数引号与空格保留，输出／输入路径由执行器独立管理。
- 双地址族确实共同进入测速，结果必须来自本次实际输入。
- 配置／Hosts 并发编辑、文件写成功但提交失败及进程中断需要明确回执。
- 原签到重复事件、周期与单次运行竞争不会造成重复测速或更新。

### Task 1: 原生安装、执行与 Hosts 规划

**Files:** Create `backend/internal/cloudflare/{settings,hosts,install,archive,runner}.go` and concentrated core tests.

**Interfaces:** `DecodeSettings(string) (Settings,error)`；`PlanHosts(string,Settings,string) (HostPlan,error)`；`Installer.Ensure(context.Context,Settings,Before) (Installation,error)`；`Run(context.Context,Installation,Settings,Before) (Result,error)`。`Before` 必须在每次对外请求、安装发布或 CLI 执行前调用，后续由运行时提交原动作意图。

- [x] 保留配置并实现参数数组、Hosts 精确替换和自动校准。
- [x] 支持官方新旧资产、归档限制、原子安装和离线回退。
- [x] 独立运行目录、合并输入、可取消执行和本次 CSV 核对；本机／Linux amd64 无 CGO 整库编译实际通过。
- [x] 首批只编译、不运行测试；运行时及 React 接通后进入 Task 3 集中验证，结果见模块实施记录。

### Task 2: 持久运行时与原插件生命周期

**Files:** Create `backend/internal/cloudflare/store*.go`, `backend/internal/httpserver/cloudflare*.go`; modify existing server/plugin/worker integration and `site_signin_worker.go`.

- [x] 原配置、管理员／Hosts 路径、代理范围、未来执行授权、调度时间与逐动作意图持久化。
- [x] 使用跨进程锁执行，未知结果不重发；Hosts 完整前后快照核对和完成事务。
- [x] 接通现代／旧插件配置、安装卸载、周期／单次、签到触发和完成通知。

### Task 3: React 与整模块集中验收

**Files:** Create `frontend/src/api/cloudflare.ts`, `frontend/src/components/CloudflareManager.tsx`; modify plugin page and tests; update migration records.

- [x] 原配置编辑、核对接管、启停、历史及原回执只读核对。
- [x] 核心／HTTP／前端集中回归、真实 React 页面、正常 Go 进程中断／重启、官方程序本机 IPv4／IPv6 协议及 Webhook 完整通知链通过；单次计划与真实签到失败消费者贯通。
- [x] 一位独立审查者核对完整调用链，集中处理发现，再验证受影响范围。
- [x] 真实结果、早期失败轮与部署／镜像等全项目剩余限制已记录；各代码批次提交推送，最终记录随本批提交核对远端。
