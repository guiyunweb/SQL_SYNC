// Package progress 定义备份/恢复过程中的进度事件，
// 引擎通过回调上报，GUI 据此实时刷新进度条与日志。
package progress

import (
	"fmt"
	"strconv"
	"time"
)

// Kind 事件类型。
type Kind int

const (
	KindLog        Kind = iota // 普通日志行
	KindTaskStart              // 某个任务（表/分片/文件）开始
	KindTaskUpdate             // 任务进度更新
	KindTaskDone               // 任务完成
	KindTaskFail               // 任务失败
	KindSummary                // 全局汇总信息
	KindOverall                // 总体进度（高频，只刷新进度条不写日志）
)

// Event 一次进度上报。
type Event struct {
	Kind    Kind      `json:"k"`
	When    time.Time `json:"t"`
	Task    string    `json:"task,omitempty"`  // 任务标识，如 "db/table#3" 或文件名
	Message string    `json:"msg,omitempty"`   // 日志内容或失败原因
	Done    int64     `json:"done,omitempty"`  // 已完成量（行数/字节数/文件数）
	Total   int64     `json:"total,omitempty"` // 预估总量，0 表示未知
}

// Reporter 进度上报回调。实现者需自行处理并发与 UI 线程切换。
type Reporter func(Event)

// Nop 空实现。
func Nop(Event) {}

// Count 把整数格式化为带千分位的字符串，如 1234567 → "1,234,567"。
func Count(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := n < 0
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// Bytes 把字节数格式化为 KB/MB/GB。
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		f /= unit
		if f < unit || u == "TB" {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return ""
}
