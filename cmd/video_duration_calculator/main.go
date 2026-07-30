package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type VideoFile struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	http.HandleFunc("/api/scan", handleScan)

	fmt.Println("Server listening on http://localhost:8081")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal(err)
	}
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	var videoFiles []string
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // ignore errors
		}
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".mp4" || ext == ".mkv" || ext == ".avi" || ext == ".mov" || ext == ".flv" || ext == ".wmv" {
				videoFiles = append(videoFiles, path)
			}
		}
		return nil
	})

	if err != nil {
		sendEvent(w, flusher, "error", map[string]string{"message": err.Error()})
		return
	}

	sort.Strings(videoFiles)

	totalFiles := len(videoFiles)
	var results []VideoFile

	sendEvent(w, flusher, "start", map[string]interface{}{"total": totalFiles})

	for i, path := range videoFiles {
		duration := getDuration(path)
		vf := VideoFile{
			Name:     filepath.Base(path),
			Path:     path,
			Duration: duration,
		}
		results = append(results, vf)

		sendEvent(w, flusher, "progress", map[string]interface{}{
			"current": i + 1,
			"total":   totalFiles,
			"file":    vf,
		})
	}

	sendEvent(w, flusher, "done", map[string]interface{}{"videos": results})
}

func getDuration(path string) float64 {
	// ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 <file>
	// 原来用的是 macOS 的 mdls（Spotlight 元数据），Linux 上没有，换成 ffprobe。
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0
	}

	// 正常输出就是一行秒数，比如 102.800000；取不到时可能是空串或 "N/A"
	valStr := strings.TrimSpace(out.String())
	if valStr == "" || valStr == "N/A" {
		return 0
	}
	duration, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0
	}
	return duration
}

func sendEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\n", eventType)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	flusher.Flush()
}
