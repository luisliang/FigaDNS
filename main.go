// main.go — FigaDNS 入口：CLI + macOS 网络配置 + 守护进程生命周期

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

const (
	version = "1.0.0"
	author  = "Merlin Studio"
)

func main() {
	// 解析全局 flags
	flag.Parse()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			cmdStart()
		case "stop":
			cmdStop()
		case "restart":
			cmdRestart()
		case "status":
			cmdStatus()
		case "setup":
			cmdSetup()
		case "uninstall":
			cmdUninstall()
		case "version", "-v", "--version":
			fmt.Printf("FigaDNS v%s by Merlin Studio\n", version)
		case "help", "-h", "--help":
			printHelp()
		default:
			// 检查是否带了 --port 参数
			if os.Args[1][0] == '-' {
				cmdStart()
			} else {
				fmt.Printf("未知命令: %s\n", os.Args[1])
				printHelp()
			}
		}
		return
	}

	// 无参数 → 直接启动（前台运行）
	cmdStart()
}

func printHelp() {
	fmt.Printf(`
FigaDNS v%s by Merlin Studio — Figma 智能 DNS 加速守护进程

用法:
  figadns [--port=5353]         启动（前台运行，用于调试）
  figadns start                 后台启动
  figadns stop                  停止
  figadns restart               重启
  figadns status                查看状态
  figadns setup                 配置 macOS 网络（一次设置）
  figadns uninstall             移除配置

参数:
  --port=<port>     DNS 监听端口 (默认: 5353)
  --upstream=<dns>  上游 DNS 服务器 (默认: 自动检测)

原理:
  监听 DNS 请求，对 *.figma.com 自动返回当前最快的服务器 IP
  后台每 30s 检测所有 Figma IP 质量，发现拥堵自动切换
  非 Figma 域名透明转发到系统 DNS
`, version)
}

func cmdStart() {
	fmt.Printf("FigaDNS v%s by Merlin Studio — Figma 智能 DNS 加速\n", version)
	fmt.Println("========================================")

	// 创建路由表
	route := NewRouteTable()

	// 创建 IP 发现器
	disc := NewIPDiscoverer()

	// 启动后台检测
	checker := NewChecker(route, disc)
	checker.Start()
	defer checker.Stop()

	// 启动 DNS 服务器
	dnsSrv := NewDNSServer(route, checker, Config.Port)
	if err := dnsSrv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动 DNS 服务器失败: %v\n", err)
		os.Exit(1)
	}
	defer dnsSrv.Stop()

	fmt.Printf("\nDNS 服务器: 127.0.0.1:%d\n", Config.Port)
	fmt.Println("状态: 运行中 (按 Ctrl+C 停止)")
	fmt.Println()

	// 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\n正在停止...")
}

const launchdLabel = "com.figadns.daemon"

func cmdStop() {
	fmt.Println("停止 FigaDNS 服务...")

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("获取用户目录失败: %v\n", err)
		return
	}
	plistFile := home + "/Library/LaunchAgents/com.figadns.daemon.plist"

	// launchctl stop 对 KeepAlive 服务只是发 SIGTERM，launchd 会立即重启。
	// 必须用 bootout/unload 才能真正移除服务。
	cmd := exec.Command("launchctl", "bootout", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/"+launchdLabel)
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("launchctl", "unload", plistFile)
		if err := cmd.Run(); err != nil {
			fmt.Printf("停止失败: %v\n", err)
			fmt.Println("提示: 服务可能未安装或未运行")
			return
		}
	}
	fmt.Println("已停止")
}

func cmdRestart() {
	fmt.Println("重启 FigaDNS 服务...")

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("获取用户目录失败: %v\n", err)
		return
	}
	plistFile := home + "/Library/LaunchAgents/com.figadns.daemon.plist"

	stopCmd := exec.Command("launchctl", "bootout", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/"+launchdLabel)
	if err := stopCmd.Run(); err != nil {
		stopCmd = exec.Command("launchctl", "unload", plistFile)
		stopCmd.Run() // 服务可能本就没运行，忽略
	}

	loadCmd := exec.Command("launchctl", "load", plistFile)
	if err := loadCmd.Run(); err != nil {
		fmt.Printf("重启失败: %v\n", err)
		fmt.Println("提示: 请先运行 `figadns setup` 安装服务")
		return
	}
	fmt.Println("已重启")
}

func cmdStatus() {
	fmt.Printf("FigaDNS v%s\n", version)
	cmd := exec.Command("launchctl", "list", launchdLabel)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println("状态: 服务未安装或未运行")
		fmt.Println("提示: 运行 `figadns setup` 或双击 setup.command 安装服务")
		return
	}
	// launchctl list 输出包含 PID（第二列），0 表示未运行
	fmt.Println("状态: 服务已安装")
	fmt.Println(string(output))
}

func cmdSetup() {
	fmt.Println("FigaDNS 配置说明")
	fmt.Println()
	fmt.Println("setup 命令仅为打印配置说明，实际安装请执行：")
	fmt.Println("  sudo ./setup.sh")
	fmt.Println("  或双击 FigaDNS.app/Contents/Resources/setup.command")
	fmt.Println()
	fmt.Println("该脚本会：")
	fmt.Println("  ① 在 /etc/resolver/ 配置本地 DNS（*.figma.com / *.figma.site 走 FigaDNS）")
	fmt.Printf("    nameserver 127.0.0.1\n    port %d\n    timeout 5\n", Config.Port)
	fmt.Println("  ② 安装 LaunchAgent（~/Library/LaunchAgents/com.figadns.daemon.plist）")
	fmt.Println("  ③ 启动后台服务（开机自启）")
}

func cmdUninstall() {
	fmt.Println("移除 FigaDNS 网络配置...")
	fmt.Println()
	fmt.Println("请运行以下命令:")
	// 收集所有 root domain（去重，避免 makeproxy-c.figma.site / makeproxy-m.figma.site
	// 重复输出，且它们由 figma.site resolver 覆盖）
	seen := make(map[string]bool)
	for _, fd := range FigmaDomains {
		if seen[fd.Domain] {
			continue
		}
		seen[fd.Domain] = true
		// makeproxy-*.figma.site 由 /etc/resolver/figma.site 覆盖，不需要单独的 resolver 文件
		if strings.HasSuffix(fd.Domain, ".figma.site") && fd.Domain != "figma.site" {
			continue
		}
		fmt.Printf("  sudo rm /etc/resolver/%s\n", fd.Domain)
	}
}
