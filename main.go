// sqlsync —— MySQL 备份/恢复工具
//
// 用法:
//
//	sqlsync                      启动桌面 GUI
//	sqlsync backup  [参数]       命令行备份
//	sqlsync restore [参数]       命令行恢复
//
// 备份输出为单个 <out>/sqlsync_<库名|multi>_<时间>.tar.gz，包内布局:
//
//	sqlsync.json                       清单（包含哪些库，恢复端只读它即可列库）
//	<库名>/schema.sql                  表结构（含视图/触发器/存储过程/事件）
//	<库名>/data/<表名>/<分片>-<序号>.sql 数据（每行一条多值 INSERT，约 8MB 一个条目）
//	<库名>/meta.json                   备份元数据（实际行数）
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "__job" { // GUI 拉起的后台任务进程
		runJobWorker(os.Args[2])
		return
	}
	enableCLIOutput() // Windows GUI 子系统构建下，CLI 子命令需要附加父控制台才有输出
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "backup":
			runBackupCLI(os.Args[2:])
			return
		case "restore":
			runRestoreCLI(os.Args[2:])
			return
		case "-h", "--help", "help":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
			printUsage()
			os.Exit(2)
		}
	}
	// 无参数：启动桌面 GUI
	runGUI()
}

func printUsage() {
	fmt.Println(`sqlsync —— MySQL 备份/恢复工具（支持 SSL、并行加速）

用法:
  sqlsync                  启动桌面 GUI
  sqlsync backup  [flags]  备份：所有库打包为单个 .tar.gz
  sqlsync restore [flags]  恢复：读取 .tar.gz，先结构后数据，并行灌入

公共连接参数:
  -host          MySQL 主机 (默认 127.0.0.1)
  -port          端口 (默认 3306)
  -u             用户 (默认 root)
  -p             密码（也可用环境变量 MYSQL_PWD）
  -ssl-mode      disable|preferred|skip-verify|verify-ca|verify-full (默认 preferred)
  -ssl-ca        CA 证书路径（verify-ca / verify-full 模式必填）

backup 参数:
  -out           备份包输出目录 (默认 ./backups)
  -databases     逗号分隔的库名；默认备份全部非系统库
  -tables        逗号分隔的表名；默认库内全部表
  -workers       并行 worker 数 (默认 8)
  -rows-per-insert 每条 INSERT 最多行数 (默认 500)
  -shard-min-rows 每个分片的目标行数；含数值单列主键的大表按主键分片 (默认 500000)
  -shard-max     单表最大分片数 (默认 16)
  -level         gzip 压缩级别 1-9 (默认 1，最快)

restore 参数:
  -file          备份包 .tar.gz（也兼容旧版备份目录；-dir 为同义参数）
  -databases     只恢复指定的库（按备份中的库名）
  -rename        库改名映射，如 old:new（可重复，逗号分隔）
  -create        目标库不存在时自动创建 (默认 true)
  -recreate      恢复前 DROP 目标库 (默认 false，危险)
  -workers       并行 worker 数 (默认 8)
  -commit-batch  每个事务提交的语句数 (默认 200)

示例:
  sqlsync backup -host 116.176.114.10 -port 24218 -u root -p xxx -ssl-mode skip-verify -out ./backups
  sqlsync restore -host 116.176.114.10 -port 24218 -u root -p xxx -file ./backups/sqlsync_db1_20261020_120000.tar.gz -rename db1:db1_copy`)
}
