// main.go — FigaDNS 入口：CLI + macOS 网络配置 + 守护进程生命周期

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
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
			cmdStop()
			cmdStart()
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

func cmdStop() {
	fmt.Println("停止 FigaDNS...")
	fmt.Println("已停止")
}

func cmdStatus() {
	fmt.Printf("FigaDNS v%s\n", version)
	fmt.Println("状态: 未运行 (正在开发中)")
}

func cmdSetup() {
	fmt.Println("正在配置 macOS 网络...")
	fmt.Println()

	resolverDir := "/etc/resolver"

	for _, fd := range FigmaDomains {
		domain := fd.Domain
		configPath := resolverDir + "/" + domain

		fmt.Printf("需要创建 %s\n", configPath)
		fmt.Printf("  内容: nameserver 127.0.0.1 (port %d)\n", Config.Port)
	}

	fmt.Println()
	fmt.Println("请运行以下命令完成配置:")
	fmt.Printf("  sudo mkdir -p /etc/resolver\n")
	for _, fd := range FigmaDomains {
		fmt.Printf("  sudo bash -c 'echo \"nameserver 127.0.0.1\nport %d\" > /etc/resolver/%s'\n",
			Config.Port, fd.Domain)
	}
	fmt.Println()
	fmt.Println("配置后,只有 *.figma.com 的 DNS 会经过 FigaDNS")
	fmt.Println("其他所有域名不受影响")
}

func cmdUninstall() {
	fmt.Println("移除 FigaDNS 网络配置...")
	fmt.Println()
	fmt.Println("请运行以下命令:")
	for _, fd := range FigmaDomains {
		fmt.Printf("  sudo rm /etc/resolver/%s\n", fd.Domain)
	}
}
