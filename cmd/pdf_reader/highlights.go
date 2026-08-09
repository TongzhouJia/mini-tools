package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 高亮存成一个 JSON 数组，跟错题本 CSV 放一起。
//
// 坐标用的是「占页面宽高的百分比」而不是像素 —— pdf.js 换缩放级别时页面像素尺寸会变，
// 存像素的话放大一次高亮就全错位了。百分比跟缩放无关，重画时乘回去就行。
const highlightsName = "高亮.json"

type hlRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type highlight struct {
	ID    string   `json:"id"`
	File  string   `json:"file"`
	Page  int      `json:"page"`
	Text  string   `json:"text"`
	Color string   `json:"color"`
	Rects []hlRect `json:"rects"`
	Time  string   `json:"time"`
}

var hlMu sync.Mutex

func highlightsPath() string { return filepath.Join(dataDir, highlightsName) }

func loadHighlights() ([]highlight, error) {
	b, err := os.ReadFile(highlightsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []highlight{}, nil
		}
		return nil, err
	}
	var out []highlight
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("高亮文件读不动（手改坏了？）：%w", err)
	}
	return out, nil
}

func saveHighlights(all []highlight) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	// 先写临时文件再改名，中途崩了不至于把整个高亮文件截断
	tmp := highlightsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, highlightsPath())
}

func handleHighlights(w http.ResponseWriter, r *http.Request) {
	hlMu.Lock()
	defer hlMu.Unlock()

	switch r.Method {
	case http.MethodGet:
		all, err := loadHighlights()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 只回当前这个文件的，前端不用自己筛
		file := r.URL.Query().Get("file")
		out := []highlight{}
		for _, h := range all {
			if file == "" || h.File == file {
				out = append(out, h)
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(out)

	case http.MethodPost:
		var h highlight
		if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
			http.Error(w, "请求体不对："+err.Error(), http.StatusBadRequest)
			return
		}
		if len(h.Rects) == 0 {
			http.Error(w, "没有可画的区域", http.StatusBadRequest)
			return
		}
		all, err := loadHighlights()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
		h.Time = time.Now().Format("2006-01-02 15:04:05")
		all = append(all, h)
		if err := saveHighlights(all); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": h.ID, "total": len(all)})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "缺 id", http.StatusBadRequest)
			return
		}
		all, err := loadHighlights()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		kept := make([]highlight, 0, len(all))
		for _, h := range all {
			if h.ID != id {
				kept = append(kept, h)
			}
		}
		if len(kept) == len(all) {
			http.Error(w, "没有这个 id", http.StatusNotFound)
			return
		}
		if err := saveHighlights(kept); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "total": len(kept)})

	default:
		http.Error(w, "只收 GET / POST / DELETE", http.StatusMethodNotAllowed)
	}
}
