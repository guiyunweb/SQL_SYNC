# sqlsync —— MySQL 备份 / 恢复工具

用 Go 编写的 MySQL 逻辑备份与恢复工具，针对 **10GB 级别大库** 做了并行加速：

- **单文件备份包**：一次备份生成一个 `sqlsync_<库名>_<时间>.tar.gz`，恢复也只需这一个文件；标准 tar / 7-Zip 可直接解开查看
- **表结构与数据分离**：包内每个库一份 `schema.sql`（表/视图/触发器/存储过程/函数/事件）+ 按表切分的数据条目
- **双向能力**：同一工具既能备份也能恢复（支持改库名恢复）
- **后台运行，适合远程桌面**：GUI 发起的任务在独立后台进程中执行，关闭窗口或断开远程桌面不会中断；重新打开 GUI 自动接上进度
- **桌面 GUI（Fyne，非 Web）**：数据库可视化勾选、总体进度（行数/速度/已用时间/预计剩余）、每个分片实时进度、日志；宽窄屏自适应布局，浅色/深色主题；同时保留 CLI 便于脚本化
- **MySQL SSL 连接**：`disable / preferred / skip-verify / verify-ca / verify-full`

## 备份加速设计

| 手段 | 说明 |
| --- | --- |
| 全局 worker 池 | 所有库的导出任务放进同一个池，按预估行数**大任务优先**调度，避免最后只剩一张大表在跑 |
| 大表主键分片 | 含数值单列主键的表按每片约 50 万行拆分（最多 16 片）并行导出 |
| 单查询流式读取 | 每个分片只发一条 `SELECT`（主键区间），结果流式读取；不再分页，无主键表也不会退化成 O(n²) 的 `LIMIT/OFFSET` |
| 零分配转义 | 行数据直接按字节转义追加到缓冲区，不经过字符串替换 |
| 并行压缩 | 每个 worker 各自压缩自己的数据条目（klauspost/compress，默认最快级别），再顺序追加进同一个 .tar.gz |
| 多值 INSERT | 每条 `INSERT` 聚合多行（默认 500 行），单条不超过 2MB（兼容 `max_allowed_packet=4MB` 的老服务器） |

## 恢复加速设计

| 手段 | 说明 |
| --- | --- |
| 流式读包 | 顺序解压 .tar.gz，读到某库 `schema.sql` 即建表，读到数据条目即交给 worker，无需先解压到磁盘 |
| 并行灌入 | 数据按约 8MB 一个条目切分，单张大表也能多 worker 并发恢复 |
| 事务批量提交 | 每 N 条语句提交一次（默认 200），`autocommit=0` |
| 会话调优 | `foreign_key_checks=0`、`unique_checks=0`、`sql_log_bin=0` |
| 逐行解析 | 每条 INSERT 独占一行，无需 SQL 语法切分器，流式读取 |

## 备份包布局

```
backups/sqlsync_mydb_20261021_093000.tar.gz
  sqlsync.json                 # 清单：包含哪些库（恢复端只读它就能列出库）
  mydb/schema.sql              # 表结构 + 视图/触发器/存储过程/事件（含 DROP IF EXISTS）
  mydb/data/orders/000-00001.sql       # 小表：整表一个分片，按约 8MB 切成多个条目
  mydb/data/big_table/003-00012.sql    # 大表：第 3 个主键分片的第 12 个条目
  mydb/meta.json               # 实际行数、耗时等元数据（位于包末尾）
```

备份包由多个 gzip member 拼接而成（每个数据条目在各自 worker 中独立压缩），这是 gzip 规范允许的格式，`tar xzf`、7-Zip、Go/Python 标准库均可直接读取。写入过程中文件名为 `*.tar.gz.partial`，成功后才改名，失败或取消会自动删除。

`schema.sql` 中每条语句用 `-- >>>STMT>>>` / `-- <<<STMT<<<` 标记包裹，触发器/存储过程体内含分号也能安全切分。

## 使用

### 发行包（dist/）

| 文件 | 平台 | 说明 |
| --- | --- | --- |
| `sqlsync-darwin-arm64` | macOS Apple Silicon | 完整 GUI + CLI |
| `sqlsync-windows-amd64.exe` | Windows x64 | 完整 GUI + CLI |
| `sqlsync-linux-amd64` | Linux x64 | **CLI-only**（纯静态、零依赖，适合服务器/容器） |

> Linux GUI 版需要在 Linux 环境编译（Fyne 依赖 X11 cgo），推荐用 [fyne-cross](https://fyne.io/fyne-cross/)（Docker）；本仓库默认发布的 Linux 包为 CLI-only。

### GUI（默认）

```bash
./sqlsync              # macOS / Windows 双击或终端启动
```

界面包含：连接配置（支持测试连接）→「备份」「恢复」页签 → 总体进度、分片进度与日志。

- **备份页**：点「从服务器获取」后勾选要备份的库（不勾 = 全部非系统库），选择输出目录即可开始；并行数、分片等在「高级参数」中
- **恢复页**：选择 .tar.gz 备份包后自动读取包内库清单，勾选要恢复的库，支持改名映射与危险操作确认
- **窗口自适应**：宽度 ≥ 980 时左右分栏；更窄时参数区在上、总体进度在下（任务/日志区隐藏）；Windows 下初始窗口按屏幕工作区缩放

#### 远程桌面下运行

点击「开始」后任务在**独立的后台进程**中执行（Windows 以 `DETACHED_PROCESS` 脱离作业对象，Linux/macOS 以 `setsid` 脱离会话），GUI 只负责显示进度：

- 关闭 GUI 窗口、**断开**远程桌面：任务继续运行；重新打开 SQLSync 会自动接上进度，期间结束的任务会显示结果
- 任务期间阻止系统睡眠（Windows）
- 任务状态与日志保存在用户配置目录 `sqlsync/jobs/<时间>/`（`events.jsonl` / `status.json`），密码只经环境变量传给后台进程，不落盘

> 注意：Windows 上「**注销**」会结束该用户的全部进程，请用关闭远程桌面窗口（断开）而不是注销；若服务器组策略设置了「结束已断开的会话」时限，也会在超时后结束会话。

### CLI

```bash
# 备份（全部非系统库）
sqlsync backup -host 116.176.114.10 -port 24218 -u root -p 'xxx' \
  -ssl-mode skip-verify -out ./backups

# 只备份指定库/表
sqlsync backup -host ... -databases shop,order -tables orders,users -out ./backups

# 恢复（-file 指定备份包；旧版目录格式同样支持）
sqlsync restore -host ... -file ./backups/sqlsync_multi_20261021_093000.tar.gz -workers 8

# 只恢复包内某个库，并改名恢复到新库
sqlsync restore -host ... -file ./backups/xxx.tar.gz -databases shop -rename shop:shop_copy

# 重建恢复（先 DROP 目标库）
sqlsync restore -host ... -file ./backups/xxx.tar.gz -recreate

# 服务器上通过 SSH 跑命令行时，可用 nohup 防止断开后被结束
nohup sqlsync backup -host ... -out ./backups > backup.log 2>&1 &
```

所有 CLI 参数见 `sqlsync --help`。

## SSL 模式

- `disable`：明文连接
- `preferred`（默认）：优先 SSL，失败退回明文
- `skip-verify`：加密但不校验证书 —— **等价于 JDBC `useSSL=true`**（连接 Azure Database for MySQL 等云托管库用这个即可）
- `verify-ca`：校验证书链（需提供 `-ssl-ca`），不校验主机名
- `verify-full`：校验证书链 + 主机名

云数据库（如 Azure 中国区 `*.mysql.database.chinacloudapi.cn`）强制 TLS，用 `skip-verify` 即可完成 TLSv1.3 加密协商（已实测 `TLS_AES_128_GCM_SHA256`）。

## 从源码构建

```bash
# macOS（GUI + CLI）
go build -o sqlsync .

# Linux CLI-only（纯静态，无 X11 依赖）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/sqlsync-linux-amd64 .

# Windows GUI（需 mingw-w64：brew install mingw-w64）
CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w -H windowsgui' -o dist/sqlsync-windows-amd64.exe .
```

纯 Go 构建无 GUI 时会自动回退为 CLI 提示（`gui_stub.go`），不影响服务器场景使用。

## 注意事项与一致性说明

- 各 worker 使用独立连接，跨表不保证全局一致快照；如需强一致，建议在低峰期执行或先锁定写入
- 无数值单列主键的表不分片，整表由一个 worker 流式导出
- 备份的 DDL 已剥离 `DEFINER`，避免跨环境恢复时因账号不存在报错
- 恢复默认 `CREATE DATABASE IF NOT EXISTS`；`-recreate` 会先 DROP 目标库，GUI 中需二次确认
