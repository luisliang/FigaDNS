// checker.go — 后台健康检测引擎：被动监听 + 主动探针

package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"
)

// Checker 健康检测器
type Checker struct {
	route   *RouteTable
	disc    *IPDiscoverer
	mu      sync.Mutex
	stopCh  chan struct{}
	running bool
	// 配置
	passiveOnly bool // 仅被动检测（不主动发包）
	activeInterval time.Duration
	tcpTimeout    time.Duration
	tlsTimeout    time.Duration
	downloadTest  bool
}

func NewChecker(route *RouteTable, disc *IPDiscoverer) *Checker {
	return &Checker{
		route:         route,
		disc:          disc,
		stopCh:        make(chan struct{}),
		activeInterval: ProbeInterval,
		tcpTimeout:    3 * time.Second,
		tlsTimeout:    5 * time.Second,
		passiveOnly:   false, // 默认开启主动探针（但低频）
	}
}

// Start 启动后台检测
func (c *Checker) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
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

	// 2. TLS 握手检测
	tlsLatency, err := measureTLSLatency(node.IP+":443", node.Domain, c.tlsTimeout)
	if err != nil {
		tlsLatency = tcpLatency * 2 // 估算
	}

	// 3. 吞吐量检测（选做，对少量 IP 执行以免费流量）
	var throughput float64 = 5.0 // 默认估值
	if c.downloadTest {
		throughput = measureThroughput(node.IP+":443", 3*time.Second)
	}

	// 丢包率估算（基于连接成功率）
	lossRate := 0.0
	if node.Metrics != nil {
		total := node.Metrics.SuccessCount + node.Metrics.FailCount + 1
		lossRate = float64(node.Metrics.FailCount) / float64(total)
		if lossRate > 0.3 {
			lossRate = 0.3
		}
	}

	metrics := Metrics{
		TCPLatency:   tcpLatency,
		TLSLatency:   tlsLatency,
		Throughput:   throughput,
		PacketLoss:   lossRate,
		LastChecked:  time.Now(),
		SuccessCount: 1,
		FailCount:    0,
	}

	c.route.Update(node.Domain, node.IP, metrics)
	c.route.ReportSuccess(node.Domain, node.IP)

	// 检查是否低于拥堵阈值，触发通知
	if node.Score < CongestionScoreThreshold {
		// 会在 printStatus 中统一报告
	}
}

// 被动检测：当有真实的 Figma 流量经过 DNS 时，记录延迟
// 由 dns.go 在转发 DNS 请求时回调
func (c *Checker) ReportPassiveLatency(domain, ip string, latency time.Duration) {
	c.route.mu.Lock()
	defer c.route.mu.Unlock()

	key := domain + ":" + ip
	node, exists := c.route.nodes[key]
	if !exists {
		return
	}

	// 指数移动平均，平滑延迟数据
	alpha := 0.3
	measured := latency.Seconds() * 1000
	if node.Metrics.TCPLatency == 0 {
		node.Metrics.TCPLatency = latency
	} else {
		ema := alpha*measured + (1-alpha)*node.Metrics.TCPLatency.Seconds()*1000
		node.Metrics.TCPLatency = time.Duration(ema) * time.Millisecond
	}
	node.Metrics.LastChecked = time.Now()
	node.Metrics.SuccessCount++
	node.Metrics.FailCount = 0
}

// 打印当前路由状态
func (c *Checker) printStatus() {
	for _, fd := range FigmaDomains {
		bestIP, bestScore := c.route.GetBest(fd.Domain)
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
func measureTLSLatency(address, serverName string, timeout time.Duration) (time.Duration, error) {
	dialer := &net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
	})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return time.Since(start) - 0, nil // TLS Dial 包含 TCP 连接时间
}

// measureThroughput 测量吞吐量（下载一个小文件）
func measureThroughput(address string, timeout time.Duration) float64 {
	// 简化实现：基于 TCP 延迟估算吞吐量
	// 实际可下载一个已知大小的资源
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return 0.5 // 保守估计
	}
	defer conn.Close()

	// 粗略估算：延迟越低，吞吐量越高
	// RTT < 50ms → ~20Mbps, RTT > 500ms → ~1Mbps
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1460)
	totalBytes := 0
	for {
		n, err := conn.Read(buf)
		totalBytes += n
		if err != nil {
			break
		}
		if time.Since(start) > timeout {
			break
		}
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		elapsed = 100 * time.Millisecond
	}
	mbps := float64(totalBytes*8) / elapsed.Seconds() / 1_000_000
	if mbps < 0.1 {
		mbps = 0.1
	}
	return mbps
}
