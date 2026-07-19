// checker.go — 后台健康检测引擎：被动监听 + 主动探针

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Checker 健康检测器
type Checker struct {
	route          *RouteTable
	disc           *IPDiscoverer
	mu             sync.Mutex
	stopCh         chan struct{}
	running        bool
	activeInterval time.Duration
	tcpTimeout     time.Duration
	tlsTimeout     time.Duration
	httpTimeout   time.Duration
}

func NewChecker(route *RouteTable, disc *IPDiscoverer) *Checker {
	return &Checker{
		route:          route,
		disc:           disc,
		stopCh:         make(chan struct{}),
		activeInterval: ProbeInterval,
		tcpTimeout:     3 * time.Second,
		tlsTimeout:     5 * time.Second,
		httpTimeout:    5 * time.Second,
	}
}

// Start 启动后台检测
func (c *Checker) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.stopCh = make(chan struct{}) // 重建 stopCh，避免复用已关闭的 channel
	c.running = true
	c.mu.Unlock()

	go c.loop()
}

// Stop 停止检测
func (c *Checker) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		close(c.stopCh)
		c.running = false
	}
}

// loop 检测主循环
func (c *Checker) loop() {
	// 启动时先做一轮快速检测
	c.runCheck()

	ticker := time.NewTicker(c.activeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.runCheck()
		case <-c.stopCh:
			return
		}
	}
}

// runCheck 执行一轮全面检测
func (c *Checker) runCheck() {
	// 先刷新 IP 列表（从 DNS 发现新 IP）
	discIPs, err := c.disc.Discover()
	if err == nil {
		for domain, ips := range discIPs {
			for _, ip := range ips {
				c.route.Register(domain, ip)
			}
		}
	}

	// 为每个域名检测候选 IP
	for _, fd := range FigmaDomains {
		domain := fd.Domain

		// 获取所有候选 IP（已发现 + 兜底）
		candidates := c.disc.GetAllIPs(domain)
		for _, ip := range candidates {
			c.route.Register(domain, ip)
		}

		// 对每个 IP 做检测
		nodes := c.route.GetAll(domain)
		var wg sync.WaitGroup
		for _, node := range nodes {
			wg.Add(1)
			go func(n *IPNode) {
				defer wg.Done()
				c.probeIP(n)
			}(node)
		}
		wg.Wait()
	}

	// 清理过期 ban
	c.route.CleanBans()

	// 打印状态
	c.printStatus()
}

// probeIP 对单个 IP 执行检测
func (c *Checker) probeIP(node *IPNode) {
	// 1. TCP 延迟检测
	tcpLatency, err := measureTCPLatency(node.IP+":443", c.tcpTimeout)
	if err != nil {
		c.route.ReportFailure(node.Domain, node.IP)
		return
	}

	// 2. TLS 握手检测（失败则视为节点不可用，不能仅靠估算）
	//    启用证书验证：ServerName 已设置，证书不匹配/链断裂会自动失败，
	//    防止 DNS 劫持场景下被误判为可用
	tlsLatency, err := measureTLSLatency(node.IP+":443", node.Domain, c.tlsTimeout)
	if err != nil {
		c.route.ReportFailure(node.Domain, node.IP)
		return
	}

	// 3. HTTPS 层探测：TCP/TLS 通不等于服务可用，可能被 WAF 拦截（403）或节点故障（502）
	//    发 HEAD 请求验证 HTTP 层，5xx 或连接失败视为节点不可用
	httpLatency, httpStatus, err := c.measureHTTP(node.IP, node.Domain, c.httpTimeout)
	if err != nil {
		c.route.ReportFailure(node.Domain, node.IP)
		return
	}
	if httpStatus >= 500 {
		c.route.ReportFailure(node.Domain, node.IP)
		return
	}
	// 4xx（如 401/403/404）属正常业务响应，节点可用
	_ = httpLatency

	// 吞吐量：默认给满分估值（20Mbps）
	// 旧的 measureThroughput 裸 TCP 读不到 HTTPS 数据恒返回 0.1，已删除
	throughput := 20.0

	// 丢包率：基于 SuccessCount/FailCount 比例（FailCount 由 ReportFailure 累积，
	// 在 Update 成功时按 0.7 衰减，避免历史失败永久拉高 lossRate）
	lossRate := 0.0
	if node.Metrics != nil {
		total := node.Metrics.SuccessCount + node.Metrics.FailCount + 1
		if total > 0 {
			lossRate = float64(node.Metrics.FailCount) / float64(total)
			if lossRate > 0.3 {
				lossRate = 0.3
			}
		}
	}

	metrics := Metrics{
		TCPLatency:  tcpLatency,
		TLSLatency:  tlsLatency,
		Throughput:  throughput,
		PacketLoss:  lossRate,
		LastChecked: time.Now(),
	}

	// Update 内部会累积 SuccessCount、衰减 FailCount
	c.route.Update(node.Domain, node.IP, metrics)
}

// 打印当前路由状态
func (c *Checker) printStatus() {
	for _, fd := range FigmaDomains {
		bestIP, bestScore := c.route.GetBest(fd.Domain)
		// 无候选 IP（域名未启用 / DNS 未发现）—— 不应显示"拥堵"
		if bestIP == "" {
			fmt.Printf("[FigaDNS] %-18s → (未发现候选 IP) %s\n", fd.Label, fd.Domain)
			continue
		}
		desc := "正常"
		if bestScore < CongestionScoreThreshold {
			desc = "⚠ 拥堵"
		} else if bestScore < 50 {
			desc = "⚡ 一般"
		} else if bestScore >= 80 {
			desc = "✅ 优质"
		}
		fmt.Printf("[FigaDNS] %-18s → %-16s (评分: %.1f) %s\n",
			fd.Label, bestIP, bestScore, desc)
	}
}

// --- 检测函数 ---

// measureTCPLatency 测量 TCP 连接耗时
func measureTCPLatency(address string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return time.Since(start), nil
}

// measureTLSLatency 测量 TLS 握手耗时
// 不再 InsecureSkipVerify：ServerName 已设置，证书链验证生效
// 防止 DNS 劫持场景下被劫持 IP 用任意证书通过探测
func measureTLSLatency(address, serverName string, timeout time.Duration) (time.Duration, error) {
	dialer := &net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
		ServerName: serverName,
	})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return time.Since(start), nil // TLS Dial 包含 TCP 连接时间
}

// measureHTTP 发送 HTTPS HEAD 请求验证 HTTP 层可用性
// 返回（延迟, HTTP 状态码, error）
// 用途：检测 WAF 拦截（403）、节点故障（502/503）等 TCP/TLS 握手无法发现的问题
//
// 指定 IP 直连：通过自定义 Transport 的 DialContext 强制连接到 node.IP，
// 同时 TLS ServerName 用 domain 保证证书验证通过
func (c *Checker) measureHTTP(ip, domain string, timeout time.Duration) (time.Duration, int, error) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		// 关键：强制连接到指定 IP，而不是 DNS 解析的结果
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr 形如 "domain:443"，替换为 "ip:443"
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			dialer := &net.Dialer{Timeout: timeout}
			return tls.DialWithDialer(dialer, network, net.JoinHostPort(ip, port), &tls.Config{
				ServerName: domain,
			})
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// 不跟随重定向：3xx 也算节点可用
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := "https://" + domain + "/"
	start := time.Now()
	resp, err := client.Head(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	return time.Since(start), resp.StatusCode, nil
}
