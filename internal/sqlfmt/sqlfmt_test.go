package sqlfmt

import "testing"

// TestAppendValue 覆盖三类列的渲染与 MySQL 字符串转义规则。
func TestAppendValue(t *testing.T) {
	cases := []struct {
		raw  []byte
		kind string
		want string
	}{
		{nil, "str", "NULL"},
		{[]byte{}, "num", "NULL"},
		{[]byte("42"), "num", "42"},
		{[]byte{0xde, 0xad}, "hex", "0xdead"},
		{[]byte{}, "hex", "''"},
		{[]byte("plain"), "str", "'plain'"},
		{[]byte("a'b\"c\\d\ne\rf\tg\x00h\x1ai\bj"), "str", `'a\'b\"c\\d\ne\rf\tg\0h\Zi\bj'`},
		{[]byte("中文"), "str", "'中文'"},
	}
	for _, c := range cases {
		if got := string(AppendValue(nil, c.raw, c.kind)); got != c.want {
			t.Errorf("AppendValue(%q,%s) = %s, want %s", c.raw, c.kind, got, c.want)
		}
	}
}
