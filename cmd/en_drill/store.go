package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Prog 是一张卡的进度。存档主键是折平后的词（Card.ID），
// 所以以后往 学习材料 里再加几集单词表，老词的进度照样接得上。
type Prog struct {
	Lvl   int    `json:"lvl"`
	Due   string `json:"due"` // YYYY-MM-DD
	Seen  int    `json:"seen"`
	Wrong int    `json:"wrong"`
	Last  string `json:"last"`
}

// 间隔按天。忘了就回 0 级，本轮还会再撞见一次。
var intervals = []int{1, 2, 4, 8, 16, 32, 64}

type Store struct {
	mu   sync.Mutex
	path string
	P    map[string]*Prog
}

func today() string { return time.Now().Format("2006-01-02") }

func plusDays(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "progress.json"), P: map[string]*Prog{}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		json.Unmarshal(b, &s.P)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// save 先写临时文件再改名。这文件是唯一一份进度，
// 半路崩了留个截断的 json 比丢档更难查。
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.P, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) get(id string) *Prog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.P[id]
}

func (s *Store) grade(id string, rating int) *Prog {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.P[id]
	if p == nil {
		p = &Prog{}
		s.P[id] = p
	}
	p.Seen++
	p.Last = today()
	switch rating {
	case 1: // 忘了
		p.Lvl = 0
		p.Wrong++
		p.Due = plusDays(1)
	case 2: // 想起来了，但慢
		p.Due = plusDays(intervals[clampLvl(p.Lvl)])
	default: // 会
		p.Lvl++
		p.Due = plusDays(intervals[clampLvl(p.Lvl)])
	}
	s.save()
	return p
}

func clampLvl(l int) int {
	if l < 0 {
		return 0
	}
	if l >= len(intervals) {
		return len(intervals) - 1
	}
	return l
}

// ── 排今天这一轮 ──────────────────────────────────────────────────────

type Item struct {
	*Card
	Prog  *Prog `json:"prog"`
	Fresh bool  `json:"fresh"`
}

type Stats struct {
	Total   int `json:"total"`
	Core    int `json:"core"`
	Extra   int `json:"extra"`
	Audio   int `json:"audio"`
	Started int `json:"started"`
	Due     int `json:"due"`
	Fresh   int `json:"fresh"`
}

func (s *Store) stats(d *Deck, tier string) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := today()
	var st Stats
	for _, c := range d.Cards {
		st.Total++
		if c.Tier == "core" {
			st.Core++
		} else {
			st.Extra++
		}
		if len(c.Hits) > 0 {
			st.Audio++
		}
		if !inTier(c, tier) {
			continue
		}
		p := s.P[c.ID]
		switch {
		case p == nil:
			st.Fresh++
		default:
			st.Started++
			if p.Due <= t {
				st.Due++
			}
		}
	}
	return st
}

func inTier(c *Card, tier string) bool {
	return tier == "all" || c.Tier == tier
}

// session 先把到期的排上，再拿新词填满。
// 到期的优先是硬规则 —— 复习永远比开新词重要，
// 反过来做的话欠账会滚成一个再也不想打开的数字。
func (s *Store) session(d *Deck, tier string, limit, fresh int) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := today()

	var due, new_ []Item
	for _, c := range d.Cards {
		if !inTier(c, tier) {
			continue
		}
		p := s.P[c.ID]
		if p == nil {
			new_ = append(new_, Item{Card: c, Fresh: true})
			continue
		}
		if p.Due <= t {
			cp := *p
			due = append(due, Item{Card: c, Prog: &cp})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].Prog.Due != due[j].Prog.Due {
			return due[i].Prog.Due < due[j].Prog.Due
		}
		return due[i].Prog.Lvl < due[j].Prog.Lvl
	})
	// 新词里让有原句的先上：那批才是这套东西真正的卖点
	sort.SliceStable(new_, func(i, j int) bool {
		return len(new_[i].Hits) > len(new_[j].Hits)
	})

	out := due
	if len(out) > limit {
		out = out[:limit]
	}
	for i := 0; i < len(new_) && i < fresh && len(out) < limit; i++ {
		out = append(out, new_[i])
	}
	return out
}
