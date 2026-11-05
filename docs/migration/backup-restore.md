# 配置和数据库离线恢复

Go 支持恢复 NAS Tools 原 ZIP 格式。归档必须包含根目录下的 `config.yaml`、`default-category.yaml`、`user.db`，其中 SQLite 必须是可独立打开的完整文件。完整和精简备份都可恢复。精简备份省略的历史无法从归档中重建。

## 校验和暂存

在 Go 独立模式登录主管理员账户，从服务页选择 ZIP 并确认上传。页面返回恢复编号。也可在使用同一配置目录和服务账户的终端运行 `nas-tools-go restore stage /path/to/backup.zip`。CLI 用 `NASTOOL_CONFIG` 指定实际应用配置。服务端限制 ZIP 为 2 GiB、数据库解压后为 8 GiB，每个配置目录同时处理一份上传。归档和配置含凭据，暂存、原文件和记录均保存在私有目录 `.nastool-restore/<编号>`。

## 离线替换和回滚

停止使用该配置目录的所有 Go 和旧版服务，再执行以下操作。锁会阻止新的 Go 进程并发打开数据库，使用旧二进制或 Python 的进程需要手动停止。容器应使用同一个 `/config` 挂载、服务账户和镜像版本运行命令。

```sh
nas-tools-go restore apply ID --offline
nas-tools-go restore status
```

命令保留原配置、分类规则、用户数据库及其 WAL、SHM、journal 文件。旧 `user.db.pre-go.bak` 和 YAML `.bak` 也归入该编号的 `previous`，使新运行实例可以建立自己的备份基线。事务按文件记录内容摘要，启动前允许用相同编号继续中断的操作或回滚。`media.db` 和媒体文件继续留在原位置。目标文件须位于支持互斥锁、同目录文件系统原子改名与目录同步的配置挂载上。

将命令中的 `ID` 换成暂存编号。选择恢复版时直接运行下方的 `resume`。选择原版时先运行 `rollback`，再运行 `resume`。

```sh
nas-tools-go restore rollback ID --offline
nas-tools-go restore resume ID --confirm-resume
```

放行恢复版前，Go 使用额外的私有临时空间检查配置和数据库能否通过启动初始化。检查失败时仍可回滚。放行后可重新启动服务。恢复数据库中的旧授权、调度和待办在启动后可能继续执行。归档和原文件由管理员离线保管，可按恢复编号核对并管理已不再需要的暂存数据。

`POST /api/v1/config/restore` 接收 ZIP 并返回暂存编号，支持原始二进制与 `file` 表单上传。旧 `file_name` 参数和 `/do` 的在线覆盖命令返回迁移提示。实际替换与回滚只接受离线 Go 命令。

集中回归通过 74 项 Go 竞态测试和 353 项前端测试。隔离 Go 进程使用原生 ZIP 验证在线拒绝替换、停机恢复、逐字节回滚、不兼容数据库的启动拦截和恢复后登录。真实存量挂载、浏览器上传交互与生产镜像仍待验收。
