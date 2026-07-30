package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	// 1. 设置下载根目录（可用 YTDLP_DIR 环境变量覆盖）
	baseDir := strings.TrimSpace(os.Getenv("YTDLP_DIR"))
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println("无法确定主目录，请用 YTDLP_DIR 环境变量指定下载目录")
			return
		}
		baseDir = filepath.Join(home, "手动选择-20260730", "Documents", "yt-dlp")
	}

	// 确保根目录存在
	err := os.MkdirAll(baseDir, os.ModePerm)
	if err != nil {
		fmt.Printf("无法创建目录 %s: %v\n", baseDir, err)
		return
	}

	// 2. 获取用户输入的 URL
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("请输入要下载的YouTube URL (支持单个视频或播放列表): ")
	url, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("读取输入失败:", err)
		return
	}

	url = strings.TrimSpace(url)
	if url == "" {
		fmt.Println("URL不能为空，程序退出。")
		return
	}

	fmt.Printf("\n⚡️ 准备下载: %s\n", url)
	fmt.Println("⏳ 正在启动 yt-dlp 并处理字幕...")

	// 3. 构建输出模板 (✨ 修复了序号渲染问题 ✨)
	playlistFolder := "%(playlist_title|)s"

	// 【关键修改】将 %02d 改为 Python的占位符格式 {:02d}，避免与 yt-dlp 自身的 % 符号冲突
	videoFolder := "%(playlist_index&{:02d} - |)s%(title)s [%(id)s]"
	fileName := "%(title)s [%(id)s].%(ext)s"

	// 最终结构
	outputTemplate := filepath.Join(baseDir, playlistFolder, videoFolder, fileName)

	// 4. 组装增强版 yt-dlp 命令
	cmdArgs := []string{
		"-f", "bv+ba/b",
		"--write-subs",
		"--write-auto-subs",
		"--sub-langs", "en", // 精准匹配纯英文字幕
		"--convert-subs", "srt",
		"-o", outputTemplate,
		"--no-mtime",
		"--sleep-subtitles", "5", // 下载字幕前随机等待，防止 429 错误
		"--sleep-requests", "2", // 请求间增加延迟
		url,
	}

	cmd := exec.Command("yt-dlp", cmdArgs...)

	// 实时输出下载进度
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 5. 执行
	err = cmd.Run()
	if err != nil {
		fmt.Printf("\n❌ 下载失败: %v\n", err)
	} else {
		fmt.Printf("\n✅ 处理完成！请在以下位置查看：\n%s\n", baseDir)
	}
}
