// config.go — 配置定义和共享上下文

package main

import (
	"context"
	"flag"
)

// 全局上下文
var ctx = context.Background()

// 运行时配置
var Config = struct {
	Port    int
	Upstream string
}{
	Port:    5353, // 默认端口
	Upstream: "",
}

func init() {
	flag.IntVar(&Config.Port, "port", 5353, "DNS 监听端口")
	flag.StringVar(&Config.Upstream, "upstream", "", "上游 DNS 服务器 (默认自动检测)")
}
