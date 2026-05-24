// figma.go — Figma 域名/IP 发现：通过 DNS 动态发现 + 已知 IP 兜底

package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// FigmaDomain 一个 Figma 域名及其子域名
type FigmaDomain struct {
	Domain string   // 主域名
	Label  string   // 显示名称
	Subs   []string // 需要加速的子域名
}

// Figma 相关域名
var FigmaDomains = []FigmaDomain{
	{
		Domain: "figma.com",
		Label:  "Figma 主站",
		Subs: []string{
			"figma.com",
			"www.figma.com",
			"static.figma.com",
			"api.figma.com",
			"graphql.figma.com",
			"fonts.figma.com",
			"images.figma.com",
		},
	},
}

// 已知 Figma IP 兜底列表 (从 FigmaNetOK 及其他来源汇总)
// 这些是 Figma/CDN 常用的 IP 段
var knownFigmaIPs = map[string][]string{
	"figma.com": {
		// Fastly CDN 常见节点
		"151.101.1.0", "151.101.65.0", "151.101.129.0", "151.101.193.0",
		"151.101.2.0", "151.101.66.0", "151.101.130.0", "151.101.194.0",
		// AWS CloudFront 节点
		"13.32.0.0", "13.224.0.0", "13.249.0.0",
		"52.84.0.0", "52.222.0.0", "54.182.0.0",
		"54.192.0.0", "54.230.0.0", "54.239.0.0",
		"204.246.0.0",
		// 亚洲节点
		"13.113.0.0", "13.115.0.0", "13.230.0.0",
		"18.176.0.0", "18.180.0.0", "18.183.0.0",
		"52.68.0.0", "52.78.0.0", "52.192.0.0",
		"52.194.0.0", "52.196.0.0", "52.198.0.0",
		"54.64.0.0", "54.65.0.0", "54.92.0.0",
		"54.150.0.0", "54.168.0.0", "54.199.0.0",
		"54.238.0.0", "54.250.0.0",
		// 日本节点
		"13.112.0.0", "13.113.0.0", "13.114.0.0",
		"13.115.0.0", "13.230.0.0", "13.231.0.0",
		"18.176.0.0", "18.177.0.0", "18.178.0.0",
		"18.179.0.0", "18.180.0.0", "18.181.0.0",
		"18.182.0.0", "18.183.0.0",
	},
}

// IPDiscoverer IP 发现器
type IPDiscoverer struct {
	mu          sync.RWMutex
	discovered  map[string][]string // domain → IPs (从 DNS 解析出来的)
	resolver    *net.Resolver
	knownOnly   bool
}

func NewIPDiscoverer() *IPDiscoverer {
	return &IPDiscoverer{
		discovered: make(map[string][]string),
		resolver:   net.DefaultResolver,
	}
}

// Discover 从 DNS 解析 Figma 域名获取 IP
func (d *IPDiscoverer) Discover() (map[string][]string, error) {
	result := make(map[string][]string)

	for _, fd := range FigmaDomains {
		for _, sub := range fd.Subs {
			ips, err := d.resolveDomain(sub)
			if err != nil {
				continue
			}
			// 合并去重
			existing := make(map[string]bool)
			for _, ip := range result[fd.Domain] {
				existing[ip] = true
			}
			for _, ip := range ips {
				if !existing[ip] {
					result[fd.Domain] = append(result[fd.Domain], ip)
					existing[ip] = true
				}
			}
		}
	}

	d.mu.Lock()
	d.discovered = result
	d.mu.Unlock()

	return result, nil
}

// resolveDomain 解析域名到 IP 列表
func (d *IPDiscoverer) resolveDomain(domain string) ([]string, error) {
	// 先尝试 A 记录
	ips, err := d.resolver.LookupHost(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", domain, err)
	}

	// 过滤只保留 IPv4
	var v4 []string
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.To4() != nil {
			v4 = append(v4, ip)
		}
	}
	return v4, nil
}

// GetAllIPs 获取所有已知 IP（已发现的 + 兜底）
func (d *IPDiscoverer) GetAllIPs(domain string) []string {
	d.mu.RLock()
	disc := d.discovered[domain]
	d.mu.RUnlock()

	// 合并兜底 IP
	fallback := knownFigmaIPs[domain]
	if fallback == nil {
		return disc
	}

	if d.knownOnly {
		return fallback
	}

	// 去重合并
	seen := make(map[string]bool)
	var combined []string
	for _, ip := range disc {
		if !seen[ip] {
			combined = append(combined, ip)
			seen[ip] = true
		}
	}
	for _, ip := range fallback {
		if !seen[ip] {
			combined = append(combined, ip)
			seen[ip] = true
		}
	}
	return combined
}

// ResolveFigmaIPs 从 Figma 域名的 DNS 响应中解析 IP
// 用于被动模式：从真实 DNS 查询中提取 IP
func (d *IPDiscoverer) ResolveFigmaIPs() []string {
	domains := []string{
		"figma.com",
		"www.figma.com",
		"static.figma.com",
		"api.figma.com",
	}

	var allIPs []string
	seen := make(map[string]bool)
	for _, domain := range domains {
		addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if !seen[addr] {
				allIPs = append(allIPs, addr)
				seen[addr] = true
			}
		}
	}
	return allIPs
}

// isFigmaDomain 判断是否 Figma 域名
func isFigmaDomain(domain string) bool {
	domain = strings.TrimSuffix(domain, ".")
	lower := strings.ToLower(domain)
	for _, fd := range FigmaDomains {
		if lower == fd.Domain || strings.HasSuffix(lower, "."+fd.Domain) {
			return true
		}
	}
	return false
}

// getFigmaRoot 获取 Figma 域名对应的根 Key
func getFigmaRoot(domain string) string {
	domain = strings.TrimSuffix(domain, ".")
	lower := strings.ToLower(domain)
	for _, fd := range FigmaDomains {
		if lower == fd.Domain || strings.HasSuffix(lower, "."+fd.Domain) {
			return fd.Domain
		}
	}
	return ""
}
