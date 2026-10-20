//go:build !windows

package main

// enableCLIOutput 非 Windows 平台无需附加控制台，直接空实现。
func enableCLIOutput() {}
