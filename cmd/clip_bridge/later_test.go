package main

import (
	"errors"
	"testing"
	"time"
)

var sgt = time.FixedZone("SGT", 8*3600)

func at(mon time.Month, day, h, m int) time.Time { return time.Date(2026, mon, day, h, m, 0, 0, sgt) }

func TestMorningAfter(t *testing.T) {
	cases := []struct {
		name   string
		first  time.Time
		minute int
		want   time.Time
	}{
		{"下午发的第一封，第二天早上", at(9, 29, 14, 0), 12, at(9, 30, 5, 12)},
		{"晚上 11 点半，还是第二天早上", at(9, 29, 23, 30), 12, at(9, 30, 5, 12)},
		{"半夜发的，挪到再下一个早上", at(9, 30, 0, 30), 12, at(10, 1, 5, 12)},
		{"凌晨 4 点多，同上", at(9, 30, 4, 37), 12, at(10, 1, 5, 12)},
		{"5 点多发的，同一早上的 5:40 也不算", at(9, 30, 5, 30), 40, at(10, 1, 5, 40)},
		{"6 点整就算白天了", at(9, 30, 6, 0), 0, at(10, 1, 5, 0)},
		{"跨月", at(9, 30, 20, 0), 59, at(10, 1, 5, 59)},
	}
	for _, c := range cases {
		if got := morningAfter(c.first, c.minute); !got.Equal(c.want) {
			t.Errorf("%s：morningAfter(%s) = %s，应该是 %s", c.name, c.first.Format("1/2 15:04"), got.Format("1/2 15:04"), c.want.Format("1/2 15:04"))
		}
	}
}

func TestAfterSend(t *testing.T) {
	plan := func() Job {
		return Job{Type: "text", Text: "x", At: [2]int64{at(9, 29, 16, 0).Unix(), at(9, 30, 5, 12).Unix()}, Next: at(9, 29, 16, 0).Unix()}
	}

	// 准点发出：第二封不动
	j := plan()
	if afterSend(&j, nil, at(9, 29, 16, 1)) || j.Sent != 1 || j.Next != at(9, 30, 5, 12).Unix() {
		t.Fatalf("准点发出后第二封应该还是 9/30 05:12，得到 sent=%d next=%s", j.Sent, when(j.Next))
	}

	// 关机到第二天 9 点才补发第一封：第二封不能跟着前后脚到，挪到再下一个早上，分钟不变
	j = plan()
	afterSend(&j, nil, at(9, 30, 9, 0))
	if j.Next != at(10, 1, 5, 12).Unix() {
		t.Fatalf("补发后第二封应该是 10/1 05:12，得到 %s", when(j.Next))
	}

	// 发失败：往后推 2 分钟重试，不算发出
	j = plan()
	now := at(9, 29, 16, 0)
	if afterSend(&j, errors.New("网络失败"), now) || j.Sent != 0 || j.Fails != 1 || j.Next != now.Add(2*time.Minute).Unix() {
		t.Fatalf("失败一次应该 2 分钟后重试，得到 sent=%d fails=%d next=%s", j.Sent, j.Fails, when(j.Next))
	}

	// 第二封发完就收工
	j = plan()
	j.Sent = 1
	if !afterSend(&j, nil, at(9, 30, 5, 12)) {
		t.Fatal("第二封发完应该返回 true")
	}
}
