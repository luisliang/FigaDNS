// route.go — IP 路由表：管理 Figma 服务器候选 IP 的质量评分与自动切换

package main

import (
	"math"
	"sync"
	"time"
)

// IP 质量评分指标
type Metrics struct {
	TCPLatency   time.Duration // TCP 连接耗时
	TLSLatency   time.Duration // TLS 握手耗时
	Throughput   float64       // 吞吐量 Mbps
	PacketLoss   float64       // 丢包率 0.0~1.0
	LastChecked  time.Time     // 上次检测时间
	SuccessCount int           // 累计成功次数（探测成功 +1）
	FailCount    int           // 累计失败次数（ReportFailure +1，Update 成功时 ×0.7 衰减）
}

// IP 节点
type IPNode struct {
	IP      string
	Domain  string // 对应的 Figma 域名
	Metrics *Metrics
	Score   float64 // 综合评分，越高越好
}

// 评分权重配置
type ScoreWeights struct {
	TCPLatencyWeight float64 // TCP 延迟权重
	TLSWeight        float64 // TLS 延迟权重
	ThroughputWeight float64 // 吞吐量权重
	LossWeight       float64 // 丢包率权重
	FreshnessWeight  float64 // 数据新鲜度权重
}

var DefaultWeights = ScoreWeights{
	TCPLatencyWeight: 0.35,
	TLSWeight:        0.15,
	ThroughputWeight: 0.30,
	LossWeight:       0.15,
	FreshnessWeight:  0.05,
}

// 拥堵阈值
const (
	CongestionScoreThreshold = 30.0 // 低于此分数视为拥堵
	FailOverThreshold        = 3    // 连续失败 N 次触发切换
	MaxFailBeforeBan         = 10   // 连续失败 N 次暂时拉黑
	BanDuration              = 5 * time.Minute
	ProbeInterval            = 30 * time.Second
	StaleDataThreshold       = 2 * time.Minute
)

// RouteTable 路由表
type RouteTable struct {
	mu       sync.RWMutex
	nodes    map[string]*IPNode // key: "domain:ip"
	best     map[string]*IPNode // key: domain → 当前最优
	banned   map[string]time.Time
	weights  ScoreWeights
}

func NewRouteTable() *RouteTable {
	return &RouteTable{
		nodes:   make(map[string]*IPNode),
		best:    make(map[string]*IPNode),
		banned:  make(map[string]time.Time),
		weights: DefaultWeights,
	}
}

// 注册候选 IP
func (rt *RouteTable) Register(domain, ip string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	key := domain + ":" + ip
	if _, exists := rt.nodes[key]; !exists {
		rt.nodes[key] = &IPNode{
			IP:     ip,
			Domain: domain,
			Metrics: &Metrics{
				LastChecked: time.Now(),
			},
			Score: 50, // 初始中立分
		}
	}
}

// 更新 IP 质量数据
func (rt *RouteTable) Update(domain, ip string, m Metrics) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	key := domain + ":" + ip
	node, exists := rt.nodes[key]
	if !exists {
		return
	}

	// 探测成功时：
	//   - 累积 SuccessCount（用于 lossRate 计算）
	//   - 衰减 FailCount（每次成功 ×0.7），避免历史失败永久拉高 lossRate
	//     衰减后 FailCount 仍保留小数，calculateScore 用 int 转换取整
	if node.Metrics != nil {
		decayedFail := float64(node.Metrics.FailCount) * 0.7
		m.FailCount = int(decayedFail + 0.5) // 四舍五入
		m.SuccessCount = node.Metrics.SuccessCount + 1
	} else {
		m.SuccessCount = 1
	}

	// 更新指标
	node.Metrics = &m

	// 计算综合评分（含失败惩罚）
	node.Score = rt.calculateScore(m)

	// 更新最优：若被更新的是当前最优且分数下降，重新选举
	current, hasBest := rt.best[domain]
	if !hasBest {
		rt.best[domain] = node
		return
	}
	if node.IP == current.IP {
		// 当前最优节点自身分数变化，重新比较
		if node.Score < current.Score {
			rt.triggerFailover(domain)
		}
		return
	}
	if node.Score > current.Score {
		rt.best[domain] = node
	}
}

// 计算综合评分 (0-100)
func (rt *RouteTable) calculateScore(m Metrics) float64 {
	// TCP 延迟评分: <50ms=100, >500ms=0
	tcpScore := 100.0 * math.Max(0, 1-m.TCPLatency.Seconds()/0.5)

	// TLS 延迟评分: <100ms=100, >1000ms=0
	tlsScore := 100.0 * math.Max(0, 1-m.TLSLatency.Seconds()/1.0)

	// 吞吐量评分: >20Mbps=100, <1Mbps=0
	throughputScore := 100.0 * math.Min(1, m.Throughput/20.0)

	// 丢包率评分: 0%=100, >20%=0
	lossScore := 100.0 * math.Max(0, 1-m.PacketLoss/0.2)

	// 新鲜度评分: 刚测=100, 2分钟后递减
	freshness := time.Since(m.LastChecked)
	freshScore := 100.0
	if freshness > StaleDataThreshold {
		freshScore = 100.0 * math.Max(0, 1-freshness.Seconds()/600.0)
	}

	score := tcpScore*rt.weights.TCPLatencyWeight +
		tlsScore*rt.weights.TLSWeight +
		throughputScore*rt.weights.ThroughputWeight +
		lossScore*rt.weights.LossWeight +
		freshScore*rt.weights.FreshnessWeight

	// 失败惩罚：每次失败扣 10 分（上限 50），持久化在 FailCount 中
	failPenalty := math.Min(50.0, float64(m.FailCount)*10.0)
	score -= failPenalty

	return math.Max(0, math.Min(100, score))
}

// GetBest 获取当前最优 IP
func (rt *RouteTable) GetBest(domain string) (string, float64) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// 检查有无被 ban 的最优
	if node, ok := rt.best[domain]; ok {
		if _, banned := rt.banned[node.IP]; !banned {
			return node.IP, node.Score
		}
	}

	// 最优被 ban 了，找次优
	var bestIP string
	var bestScore float64
	for _, node := range rt.nodes {
		if node.Domain != domain {
			continue
		}
		if _, banned := rt.banned[node.IP]; banned {
			continue
		}
		if bestIP == "" || node.Score > bestScore {
			bestIP = node.IP
			bestScore = node.Score
		}
	}
	return bestIP, bestScore
}

// ReportFailure 报告失败，触发自动切换/ban
func (rt *RouteTable) ReportFailure(domain, ip string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	key := domain + ":" + ip
	node, exists := rt.nodes[key]
	if !exists {
		return
	}

	node.Metrics.FailCount++
	node.Metrics.SuccessCount = 0
	// 重新计算评分（calculateScore 已含 FailCount 惩罚，持久化生效）
	node.Score = rt.calculateScore(*node.Metrics)

	if node.Metrics.FailCount >= MaxFailBeforeBan {
		rt.banned[ip] = time.Now().Add(BanDuration)
	}

	// 连续失败达到 FailOverThreshold 且当前最优就是这个，触发切换
	if best, ok := rt.best[domain]; ok && best.IP == ip && node.Metrics.FailCount >= FailOverThreshold {
		rt.triggerFailover(domain)
	}
}

// triggerFailover 触发切换：找到次优 IP 提升为最优
func (rt *RouteTable) triggerFailover(domain string) {
	var bestNode *IPNode
	for _, node := range rt.nodes {
		if node.Domain != domain {
			continue
		}
		if _, banned := rt.banned[node.IP]; banned {
			continue
		}
		if bestNode == nil || node.Score > bestNode.Score {
			bestNode = node
		}
	}
	if bestNode != nil {
		rt.best[domain] = bestNode
	}
}

// Unban 解封 IP
func (rt *RouteTable) Unban(ip string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.banned, ip)
}

// 定期清理过期 ban
func (rt *RouteTable) CleanBans() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	for ip, until := range rt.banned {
		if now.After(until) {
			delete(rt.banned, ip)
		}
	}
}

// GetAll 获取某个域名的所有候选 IP
func (rt *RouteTable) GetAll(domain string) []*IPNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var nodes []*IPNode
	for _, node := range rt.nodes {
		if node.Domain == domain {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// GetDomains 获取所有管理的域名
func (rt *RouteTable) GetDomains() []string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	seen := make(map[string]bool)
	var domains []string
	for _, node := range rt.nodes {
		if !seen[node.Domain] {
			seen[node.Domain] = true
			domains = append(domains, node.Domain)
		}
	}
	return domains
}
