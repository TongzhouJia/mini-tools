package main

import "testing"

func TestSplitTitle(t *testing.T) {
	cases := []struct{ in, en, zh string }{
		{"Permissive宽容的", "Permissive", "宽容的"},
		{"Demonstrate证明", "Demonstrate", "证明"},
		{"look up 查阅", "look up", "查阅"},
		{"run - 跑", "run", "跑"},
		{"revert：恢复", "revert", "恢复"},
		{"  persistent  执着的  ", "persistent", "执着的"},
		{"deadline", "deadline", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		en, zh := splitTitle(c.in)
		if en != c.en || zh != c.zh {
			t.Errorf("splitTitle(%q) = (%q, %q)，想要 (%q, %q)", c.in, en, zh, c.en, c.zh)
		}
	}
}

// 前端的 start/end 是 UTF-16 下标，Go 的是字节。句子里带中文时两者会岔开，
// 这个测试就是钉死这一点——岔开了高亮会跑到别的字上。
func TestLocateWordUTF16(t *testing.T) {
	cases := []struct {
		sentence, word string
		start, end     int
	}{
		{"Put SELinux into permissive mode.", "permissive", 17, 27},
		{"这个单词造的句子Sample", "Sample", 8, 14},   // 8 个汉字 = 8 个 code unit
		{"他说 revert 是恢复的意思", "revert", 3, 9}, // 前面 2 汉字 + 1 空格
		{"Demonstrate that it works", "demonstrate", 0, 11}, // 大小写无关
		{"这个单词造的句子Sample", "Demonstrate", 0, 0},           // 句子里没有 = 不定位
		{"", "word", 0, 0},
	}
	for _, c := range cases {
		s, e := locateWord(c.sentence, c.word)
		if s != c.start || e != c.end {
			t.Errorf("locateWord(%q, %q) = (%d, %d)，想要 (%d, %d)", c.sentence, c.word, s, e, c.start, c.end)
		}
	}
}
