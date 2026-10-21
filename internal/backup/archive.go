package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
)

// archiveWriter 把备份写成单个 .tar.gz。
//
// 每个条目在调用方 goroutine 里独立压缩成一个 gzip member（tar 头 + 内容 + 补齐），
// 再加锁顺序追加到文件。gzip 规范允许多个 member 直接拼接，解压结果即完整 tar 流，
// 因此 tar/7-Zip/Go 标准库都能直接读取，同时压缩在多个 worker 上并行进行。
type archiveWriter struct {
	path    string // 最终文件名
	tmpPath string // 写入中的临时文件名，成功后改名
	level   int

	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	written int64

	bufPool sync.Pool
}

func newArchiveWriter(path string, level int) (*archiveWriter, error) {
	tmp := path + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	a := &archiveWriter{path: path, tmpPath: tmp, level: level, f: f, w: bufio.NewWriterSize(f, 4<<20)}
	a.bufPool.New = func() any { return new(bytes.Buffer) }
	return a, nil
}

// addFile 压缩并追加一个条目；可被多个 goroutine 并发调用。
func (a *archiveWriter) addFile(name string, data []byte) error {
	buf := a.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer a.bufPool.Put(buf)

	gz, err := gzip.NewWriterLevel(buf, a.level)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Now(),
		Format:  tar.FormatPAX, // 支持超长/中文文件名
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil { // 只补齐到 512 字节边界，不写结束块
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	n, err := a.w.Write(buf.Bytes())
	a.written += int64(n)
	return err
}

// size 返回已写入的压缩字节数。
func (a *archiveWriter) size() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.written
}

// finish 写入 tar 结束块并把临时文件改名为最终文件。
func (a *archiveWriter) finish() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	gz, err := gzip.NewWriterLevel(a.w, a.level)
	if err != nil {
		return err
	}
	if err := tar.NewWriter(gz).Close(); err != nil { // 两个全零块
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := a.w.Flush(); err != nil {
		return err
	}
	if err := a.f.Sync(); err != nil {
		return err
	}
	if err := a.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(a.tmpPath, a.path); err != nil {
		return fmt.Errorf("重命名备份包失败: %w", err)
	}
	return nil
}

// abort 失败时关闭并删除未完成的临时文件。
func (a *archiveWriter) abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.f.Close()
	os.Remove(a.tmpPath)
}
