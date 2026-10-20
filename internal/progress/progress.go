// Package progress 定义备份/恢复过程中的进度事件，
// 引擎通过回调上报，GUI 据此实时刷新进度条与日志。
package progress

import "time"

// Kind 事件类型。
type Kind int

const (
	KindLog        Kind = iota // 普通日志行
	KindTaskStart              // 某个任务（表/分片/文件）开始
	KindTaskUpdate             // 任务进度更新
	KindTaskDone               // 任务完成
	KindTaskFail               // 任务失败
	KindSummary                // 全局汇总信息
)

// Event 一次进度上报。
type Event struct {
	Kind    Kind
	When    time.Time
	Task    string // 任务标识，如 "db/table#3" 或文件名
	Message string // 日志内容或失败原因
	Done    int64  // 已完成行数/文件数
	Total   int64  // 预估总行数/总文件数，0 表示未知
}

// Reporter 进度上报回调。实现者需自行处理并发与 UI 线程切换。
type Reporter func(Event)

// Nop 空实现。
func Nop(Event) {}
