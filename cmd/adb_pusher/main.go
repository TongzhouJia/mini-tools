package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = `adb_pusher —— 往手机推文件，推完手机上弹通知告诉你成没成

用法：
  adb_pusher ~/Downloads/xxx.mp4   直接推这个文件或整个文件夹
  adb_pusher                       不给参数就交互式问你要推啥

推到哪：手机的 /sdcard/download/（写死的，改要动代码）
方向：本机 → 手机。反过来从手机拷东西用 adb pull

依赖：adb，且手机已经开好 USB 调试并授权过这台电脑
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		}
	}

	fmt.Println("=== ADB 文件传输工具 ===")

	// 1. 检查本地环境是否有 adb
	if _, err := exec.LookPath("adb"); err != nil {
		fmt.Println("错误: 未找到 adb 命令，请确保已安装并配置了环境变量。")
		os.Exit(1)
	}

	var path string

	// 2. 获取路径（参数或交互式）
	if len(os.Args) > 1 {
		path = os.Args[1]
	} else {
		fmt.Print("请输入要传输的文件或文件夹路径: ")
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("读取输入失败:", err)
			return
		}
		path = input
	}

	path = strings.TrimSpace(path)
	path = strings.Trim(path, `"'`)

	if path == "" {
		fmt.Println("路径不能为空，已退出。")
		return
	}

	// 3. 验证本地文件是否存在
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("错误: 本地文件或目录 '%s' 不存在。\n", path)
		return
	}

	target := "/sdcard/download/"
	fmt.Printf("正在将 '%s' 传输到 '%s'...\n", path, target)

	// 4. 执行 adb push 并捕获错误
	cmd := exec.Command("adb", "push", path, target)
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	if err := cmd.Run(); err != nil {
		errorMsg := strings.TrimSpace(stderr.String())
		if errorMsg == "" {
			errorMsg = err.Error()
		}
		fmt.Println("传输失败:", err)

		// 手机弹窗提示失败
		notifyTitle := "❌ 文件传输失败"
		notifyText := fmt.Sprintf("错误原因: %s", errorMsg)
		exec.Command("adb", "shell", "cmd", "notification", "post", "-S", "bigtext", "-t", notifyTitle, "ADB_PUSHER", notifyText).Run()
	} else {
		fmt.Println("传输完成!")

		// 手机弹窗提示成功
		fileName := filepath.Base(path)
		notifyTitle := "✅ 文件传输成功"
		notifyText := fmt.Sprintf("文件 '%s' 已成功传输至 %s", fileName, target)
		exec.Command("adb", "shell", "cmd", "notification", "post", "-S", "bigtext", "-t", notifyTitle, "ADB_PUSHER", notifyText).Run()
	}
}
