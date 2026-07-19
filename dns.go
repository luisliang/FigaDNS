// dns.go — DNS 服务器：拦截 Figma 查询，返回最优 IP；其他透传

package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// DNSServer DNS 服务器
type DNSServer struct {
	route       *RouteTable
	checker     *Checker
	udpServer   *dns.Server
	tcpServer   *dns.Server
	upstreamDNS string // 上游 DNS 服务器
	port        int
	mu          sync.Mutex
	running     bool

	// DNS 缓存
	cache    map[string]dnsCacheEntry
	cacheMu  sync.RWMutex
	cacheTTL time.Duration
	stopCh   chan struct{} // 停止缓存清理 goroutine
}

type dnsCacheEntry struct {
	msg     *dns.Msg
	expires time.Time
}

func NewDNSServer(route *RouteTable, checker *Checker, port int) *DNSServer {
	// 优先使用命令行 --upstream 参数，否则自动检测系统 DNS
	upstream := Config.Upstream
	if upstream == "" {
		upstream = detectSystemDNS()
	}

	s := &DNSServer{
		route:       route,
		checker:     checker,
		upstreamDNS: upstream,
		port:        port,
		cache:       make(map[string]dnsCacheEntry),
		// 30s 平衡新鲜度与上游负载：非 Figma 域名不做 IP 优选，可缓存更久
		cacheTTL: 30 * time.Second,
		stopCh:   make(chan struct{}),
	}

	// 启动缓存清理 goroutine
	go s.cacheCleanup()

	return s
}

// cacheCleanup 定期清理过期缓存条目，防止内存泄漏
func (s *DNSServer) cacheCleanup() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cacheMu.Lock()
			now := time.Now()
			for k, v := range s.cache {
				if now.After(v.expires) {
					delete(s.cache, k)
				}
			}
			s.cacheMu.Unlock()
		case <-s.stopCh:
			return
		}
	}
}

// detectSystemDNS 检测系统 DNS
func detectSystemDNS() string {
	// 读 /etc/resolv.conf
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err == nil && len(config.Servers) > 0 {
		return net.JoinHostPort(config.Servers[0], config.Port)
	}
	// fallback: 阿里公共 DNS（国内可用，海外也可达）
	return "223.5.5.5:53"
}

// Start 启动 DNS 服务器
func (s *DNSServer) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = true
	s.mu.Unlock()

	handler := dns.NewServeMux()
	handler.HandleFunc(".", s.handleQuery)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)

	udpStarted := make(chan struct{})
	tcpStarted := make(chan struct{})

	s.udpServer = &dns.Server{
		Addr:              addr,
		Net:               "udp",
		Handler:           handler,
		UDPSize:           65535,
		NotifyStartedFunc: func() { close(udpStarted) },
	}
	s.tcpServer = &dns.Server{
		Addr:              addr,
		Net:               "tcp",
		Handler:           handler,
		NotifyStartedFunc: func() { close(tcpStarted) },
	}

	udpErrCh := make(chan error, 1)
	tcpErrCh := make(chan error, 1)

	go func() { udpErrCh <- s.udpServer.ListenAndServe() }()
	go func() { tcpErrCh <- s.tcpServer.ListenAndServe() }()

	// 等待 UDP 启动或失败（端口冲突时不能静默返回 nil）
	if err := waitStarted(udpStarted, udpErrCh, "UDP", s.port); err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return err
	}
	fmt.Printf("[FigaDNS] DNS UDP 服务器已启动 on %s\n", addr)

	// 等待 TCP 启动或失败
	if err := waitStarted(tcpStarted, tcpErrCh, "TCP", s.port); err != nil {
		s.udpServer.Shutdown()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return err
	}

	return nil
}

// waitStarted 等待 DNS 服务器启动，超时或出错时返回 error
func waitStarted(startedCh chan struct{}, errCh chan error, proto string, port int) error {
	select {
	case <-startedCh:
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("%s 服务器启动失败: %w（端口 %d 可能被占用）", proto, err, port)
		}
		return fmt.Errorf("%s 服务器意外退出", proto)
	case <-time.After(2 * time.Second):
		return fmt.Errorf("%s 服务器启动超时（端口 %d 可能被占用）", proto, port)
	}
}

// Stop 停止 DNS 服务器
func (s *DNSServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	if s.udpServer != nil {
		s.udpServer.Shutdown()
	}
	if s.tcpServer != nil {
		s.tcpServer.Shutdown()
	}
	close(s.stopCh) // 停止缓存清理 goroutine
	s.running = false
}

// handleQuery 处理 DNS 查询
func (s *DNSServer) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		return
	}

	q := r.Question[0]
	domain := strings.TrimSuffix(q.Name, ".")

	// 只处理 A 记录查询
	// 已知限制：AAAA（IPv6）查询直接转发上游，不参与 IP 优选。
	// 双栈网络下浏览器会优先 IPv6，IPv4 加速可能被绕过。
	// 当前 FigaDNS 只做 IPv4 加速；IPv6 探测与优选作为未来扩展。
	if q.Qtype != dns.TypeA {
		s.forwardToUpstream(w, r)
		return
	}

	// 判断是否 Figma 域名
	if !isFigmaDomain(domain) {
		s.forwardToUpstream(w, r)
		return
	}

	// 用根域名查询路由表（www.figma.com → figma.com）
	rootDomain := getFigmaRoot(domain)
	if rootDomain == "" {
		rootDomain = domain
	}

	// Figma 域名 → 从路由表返回最优 IP
	bestIP, score := s.route.GetBest(rootDomain)

	// 如果没有数据，转发上游（forwardToUpstream 会从响应中学习 IP，无需重复查询）
	if bestIP == "" {
		s.forwardToUpstream(w, r)
		return
	}

	// 有数据，返回最优 IP
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	ip := net.ParseIP(bestIP)
	if ip == nil {
		s.forwardToUpstream(w, r)
		return
	}

	ttl := uint32(30) // 短 TTL，便于快速切换
	m.Answer = append(m.Answer, &dns.A{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		A: ip,
	})

	status := "正常"
	if score < CongestionScoreThreshold {
		status = "⚠拥堵"
	} else if score < 50 {
		status = "⚡一般"
	}
	fmt.Printf("[FigaDNS] %-30s → %-16s (%.0f分) %s\n",
		domain, bestIP, score, status)

	w.WriteMsg(m)
}

// forwardToUpstream 转发到上游 DNS
func (s *DNSServer) forwardToUpstream(w dns.ResponseWriter, r *dns.Msg) {
	client := &dns.Client{
		Timeout: 5 * time.Second,
	}

	// 检查缓存
	cacheKey := r.Question[0].String()
	s.cacheMu.RLock()
	entry, found := s.cache[cacheKey]
	s.cacheMu.RUnlock()

	var resp *dns.Msg
	var err error

	if found && time.Now().Before(entry.expires) {
		// 缓存命中：必须 Copy，否则后续修改 resp.Id 会污染缓存
		resp = entry.msg.Copy()
	} else {
		resp, _, err = client.Exchange(r, s.upstreamDNS)
		if err != nil {
			fmt.Printf("[FigaDNS] 上游 DNS 错误: %v\n", err)
			dns.HandleFailed(w, r)
			return
		}

		q := r.Question[0]
		learnDomain := strings.TrimSuffix(q.Name, ".")

		// 写入缓存（存副本，避免后续修改 resp.Id 影响缓存）
		// Figma 域名缓存短（IP 可能切换），非 Figma 域名缓存久（减少上游查询）
		cacheTTL := 300 * time.Second
		if isFigmaDomain(learnDomain) {
			cacheTTL = 30 * time.Second
		}
		s.cacheMu.Lock()
		s.cache[cacheKey] = dnsCacheEntry{
			msg:     resp.Copy(),
			expires: time.Now().Add(cacheTTL),
		}
		s.cacheMu.Unlock()

		if q.Qtype == dns.TypeA && isFigmaDomain(learnDomain) {
			rootDomain := getFigmaRoot(learnDomain)
			if rootDomain != "" {
				for _, ans := range resp.Answer {
					if a, ok := ans.(*dns.A); ok {
						s.route.Register(rootDomain, a.A.String())
					}
				}
			}
		}
	}

	// 确保响应 Id 与请求匹配（缓存命中时 resp 来自历史请求，Id 必须重置）
	resp.Id = r.Id
	w.WriteMsg(resp)
}
