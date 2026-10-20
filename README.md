# sqlsync —— MySQL 备份 / 恢复工具

用 Go 编写的 MySQL 逻辑备份与恢复工具，针对 **10GB 级别大库** 做了并行加速：

- **表结构与数据分离备份**：结构（表/视图/触发器/存储过程/函数/事件）写入 `schema.sql`，数据按表写入 `data/*.sql.gz`
- **双向能力**：同一工具既能备份也能恢复（支持改库名恢复）
- **桌面 GUI（Fyne，非 Web）**：连接配置、**数据库可视化勾选**（备份页一键从服务器拉取库列表，恢复页扫描备份目录）、页签式操作、每表实时进度条、日志窗口；同时保留 CLI 便于脚本化
- **MySQL SSL 连接**：`disable / preferred / skip-verify / verify-ca / verify-full`

## 备份加速设计

| 手段 | 说明 |
| --- | --- |
| 多表并行 | worker 池并发导出多张表 |
| 大表主键分片 | 超过阈值（默认 200 万行）且含数值单列主键时，按主键范围拆成最多 N 片并行导出 |
| 键集分页 | 分片内按 `pk > lastPK ORDER BY pk LIMIT ?` 翻页，避免大 OFFSET 的全表扫描 |
| 多值 INSERT | 每条 `INSERT` 聚合多行（默认 500 行），并受单条字节上限约束 |
| gzip 压缩 | 默认开启，10G 数据落盘体积显著缩小 |
| 块式读取 | 每次 SELECT 固定行数（默认 2000），内存占用恒定 |

## 恢复加速设计

| 手段 | 说明 |
| --- | --- |
| 并行灌入 | 数据文件多 worker 并发执行 |
| 事务批量提交 | 每 N 条语句提交一次（默认 200），`autocommit=0` |
| 会话调优 | `foreign_key_checks=0`、`unique_checks=0`、`sql_log_bin=0` |
| 逐行解析 | 每条 INSERT 独占一行，无需 SQL 语法切分器，流式读取 |

## 输出布局

```
backups/
  mydb/
    schema.sql              # 表结构 + 视图/触发器/存储过程/事件（含 DROP IF EXISTS）
    meta.json               # 库字符集、表行数、耗时等元数据
    data/
      orders.sql.gz         # 小表单文件
      big_table#001.sql.gz  # 大表按主键分片
      big_table#002.sql.gz
```

`schema.sql` 中每条语句用 `-- >>>STMT>>>` / `-- <<<STMT<<<` 标记包裹，触发器/存储过程体内含分号也能安全切分。

## 使用

### GUI（默认）

```bash
go run .
# 或编译后
go build -o sqlsync . && ./sqlsync
```

界面包含：连接配置（支持测试连接）→「备份」「恢复」页签 → 实时进度与日志。

- **备份页**：点「从服务器获取数据库列表」后勾选要备份的库（不勾 = 全部非系统库），支持全选/清空
- **恢复页**：选择备份目录后自动扫描（或点「扫描目录」）勾选要恢复的库，支持改名映射与危险操作确认

### CLI

```bash
# 备份（全部非系统库）
sqlsync backup -host 116.176.114.10 -port 24218 -u root -p 'xxx' \
  -ssl-mode skip-verify -out ./backups

# 只备份指定库/表
sqlsync backup -host ... -databases shop,order -tables orders,users -out ./backups

# 恢复
sqlsync restore -host ... -dir ./backups -workers 8

# 改名恢复（恢复到新库）
sqlsync restore -host ... -dir ./backups -rename shop:shop_copy

# 重建恢复（先 DROP，会确认）
sqlsync restore -host ... -dir ./backups -recreate
```

所有 CLI 参数见 `sqlsync --help`。

## SSL 模式

- `disable`：明文连接
- `preferred`（默认）：优先 SSL，失败退回明文
- `skip-verify`：加密但不校验证书
- `verify-ca`：校验证书链（需提供 `-ssl-ca`），不校验主机名
- `verify-full`：校验证书链 + 主机名

## 注意事项与一致性说明

- 各 worker 使用独立连接，跨表不保证全局一致快照；如需强一致，建议在低峰期执行或先锁定写入
- 无主键且无唯一键的表退回 `LIMIT/OFFSET` 分页，大表上较慢且并发写入时可能重复/遗漏
- 备份的 DDL 已剥离 `DEFINER`，避免跨环境恢复时因账号不存在报错
- 恢复默认 `CREATE DATABASE IF NOT EXISTS`；`-recreate` 会先 DROP 目标库，GUI 中需二次确认
