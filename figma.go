// figma.go — Figma 域名/IP 发现：通过 DNS 动态发现 + 已知 IP 兜底

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// FigmaDomain 一个 Figma 域名及其子域名
type FigmaDomain struct {
	Domain string   // 主域名
	Label  string   // 显示名称
	Subs   []string // 需要加速的子域名
}

// Figma 相关域名
// 依据 Figma 官方网络策略（Help Center: Adjust your network settings）
//   *.figma.com             主站 / API / 静态资源
//   *.figma.site            原型预览 / 嵌入 / Make Proxy
//   *.makeproxy-c.figma.site  实时协作代理（C 线路）— 独立 root，IP 池与 figma.site 不同
//   *.makeproxy-m.figma.site  实时协作代理（M 线路）— 独立 root
// 不纳入：
//   figma.app — 桌面应用标识，无公网 A 记录（8.8.8.8 验证为 SERVFAIL）
//   awswaf.com / esm.sh / jsdelivr.net — 第三方公共 CDN，与 Figma 不共享 IP 池
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
	{
		Domain: "figma.site",
		Label:  "Figma 原型/嵌入",
		Subs: []string{
			"figma.site",
		},
	},
	{
		// Make Proxy 是 Figma 实时协作代理，独立 IP 池，必须单独 root
		// 不能归到 figma.site，否则会污染 figma.site 的路由表
		Domain: "makeproxy-c.figma.site",
		Label:  "Figma 协作代理 C",
		Subs: []string{
			"makeproxy-c.figma.site",
		},
	},
	{
		Domain: "makeproxy-m.figma.site",
		Label:  "Figma 协作代理 M",
		Subs: []string{
			"makeproxy-m.figma.site",
		},
	},
}

// 已知 Figma IP 兜底列表（真实可达的 CloudFront 边缘节点 IP，非网段号）
// 用于 DNS 解析失败时的兜底，避免候选 IP 列表为空
var knownFigmaIPs = map[string][]string{
	"figma.com": {
		"13.224.0.132",
		"13.224.0.171",
		"13.224.0.245",
		"108.156.210.132",
		"108.156.210.171",
	},
	"figma.site": {
		"13.224.0.132",
		"13.224.0.171",
		"108.156.210.132",
	},
}

// IPDiscoverer IP 发现器
type IPDiscoverer struct {
	mu         sync.RWMutex
	discovered map[string][]string // domain → IPs (从 DNS 解析出来的)
	resolver   *net.Resolver
}

// chinaDNS 用于 IP 发现的可靠 DNS（阿里公共 DNS，国内外均可达）
var chinaDNS = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, "udp", "223.5.5.5:53")
	},
}

func NewIPDiscoverer() *IPDiscoverer {
	return &IPDiscoverer{
		discovered: make(map[string][]string),
		resolver:   chinaDNS,
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
// 必须按域名长度降序匹配，否则 makeproxy-c.figma.site 会被错误归到 figma.site
// （两者 IP 池不同，混在一起会污染路由表）
func getFigmaRoot(domain string) string {
	domain = strings.TrimSuffix(domain, ".")
	lower := strings.ToLower(domain)

	var bestMatch string
	for _, fd := range FigmaDomains {
		if lower == fd.Domain || strings.HasSuffix(lower, "."+fd.Domain) {
			if len(fd.Domain) > len(bestMatch) {
				bestMatch = fd.Domain
			}
		}
	}
	return bestMatch
}
