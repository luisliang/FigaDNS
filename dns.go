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
	upstreamDNS string    // 上游 DNS 服务器
	port        int
	mu          sync.Mutex
	running     bool

	// DNS 缓存
	cache     map[string]dnsCacheEntry
	cacheMu   sync.RWMutex
	cacheTTL  time.Duration
}

type dnsCacheEntry struct {
	msg     *dns.Msg
	expires time.Time
}

func NewDNSServer(route *RouteTable, checker *Checker, port int) *DNSServer {
	// 自动检测系统 DNS
	upstream := detectSystemDNS()

	return &DNSServer{
		route:       route,
		checker:     checker,
		upstreamDNS: upstream,
		port:        port,
		cache:       make(map[string]dnsCacheEntry),
		cacheTTL:    10 * time.Second,
	}
}

// detectSystemDNS 检测系统 DNS
func detectSystemDNS() string {
	// 读 /etc/resolv.conf
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err == nil && len(config.Servers) > 0 {
		return net.JoinHostPort(config.Servers[0], config.Port)
	}
	// fallback
	return "8.8.8.8:53"
}

// Start 启动 DNS 服务器
func (s *DNSServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	handler := dns.NewServeMux()
	handler.HandleFunc(".", s.handleQuery)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)

	s.udpServer = &dns.Server{
		Addr:    addr,
		Net:     "udp",
		Handler: handler,
		UDPSize: 65535,
	}
	s.tcpServer = &dns.Server{
		Addr:    addr,
		Net:     "tcp",
		Handler: handler,
	}

	s.running = true

	// 启动 UDP
	go func() {
		fmt.Printf("[FigaDNS] DNS UDP 服务器已启动 on %s\n", addr)
		if err := s.udpServer.ListenAndServe(); err != nil {
			fmt.Printf("[FigaDNS] UDP 服务器错误: %v\n", err)
		}
	}()

	// 启动 TCP
	go func() {
		if err := s.tcpServer.ListenAndServe(); err != nil {
			fmt.Printf("[FigaDNS] TCP 服务器错误: %v\n", err)
		}
	}()

	return nil
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

	// 如果没有数据，先转发上游，同时记录 IP
	if bestIP == "" {
		// 异步转发并学习
		go s.learnFromUpstream(rootDomain)
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

	// 记录被动延迟数据（从 DNS 查询响应时间估算）
	if s.checker != nil {
		startTime, ok := w.RemoteAddr().(*net.UDPAddr)
		if ok {
			_ = startTime // 简化处理
		}
		// 无法精确获取，依赖主动检测
	}

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
		resp = entry.msg
	} else {
		resp, _, err = client.Exchange(r, s.upstreamDNS)
		if err != nil {
			fmt.Printf("[FigaDNS] 上游 DNS 错误: %v\n", err)
			dns.HandleFailed(w, r)
			return
		}
		// 写入缓存
		s.cacheMu.Lock()
		s.cache[cacheKey] = dnsCacheEntry{
			msg:     resp,
			expires: time.Now().Add(s.cacheTTL),
		}
		s.cacheMu.Unlock()
	}

	w.WriteMsg(resp)
}

// learnFromUpstream 向上游学习 Figma IP
func (s *DNSServer) learnFromUpstream(domain string) {
	client := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)

	resp, _, err := client.Exchange(m, s.upstreamDNS)
	if err != nil || resp == nil {
		return
	}

	for _, ans := range resp.Answer {
		if a, ok := ans.(*dns.A); ok {
			root := getFigmaRoot(domain)
			if root != "" {
				s.route.Register(root, a.A.String())
			}
		}
	}
}

// SetUpstream 动态设置上游 DNS
func (s *DNSServer) SetUpstream(dns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamDNS = dns
}
