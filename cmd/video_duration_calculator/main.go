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
	// mdls -name kMDItemDurationSeconds <file>
	cmd := exec.Command("mdls", "-name", "kMDItemDurationSeconds", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return 0
	}

	output := strings.TrimSpace(out.String())
	// Expected output: kMDItemDurationSeconds = 102.8
	// Sometimes it might return "(null)" if it can't read it
	parts := strings.Split(output, "=")
	if len(parts) == 2 {
		valStr := strings.TrimSpace(parts[1])
		if valStr != "(null)" {
			duration, err := strconv.ParseFloat(valStr, 64)
			if err == nil {
				return duration
			}
		}
	}
	return 0
}

func sendEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\n", eventType)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	flusher.Flush()
}
