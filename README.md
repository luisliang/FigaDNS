# FigaDNS 🚀

**Figma 智能 DNS 加速工具** — 自动检测 Figma 各 CDN 节点的网络拥堵，动态切换最优线路。

由 [Merlin Studio](https://github.com/MerlinStudio) 出品，替代传统 hosts 静态绑定方案。

---

## 为什么需要它？

Figma 使用 AWS CloudFront 等 CDN 分发资源，不同地区的 CDN 节点延迟差异巨大。
**FigaDNS** 的方案：

- 本地运行一个轻量 DNS 服务器
- 持续检测多个 Figma CDN 节点的 **TCP 延迟、TLS 握手速度、吞吐量**
- 每次 DNS 查询自动返回当前**评分最高**的节点
- 拥堵时秒级自动切换，无需人工干预

---

## 快速开始

### 1. 安装

```bash
# 把 FigaDNS.app 拖到「应用程序」文件夹
# 然后双击运行 setup.command（只需一次）
```

或手动执行：

```bash
sudo ./setup.sh
```

### 2. 启动

双击 `FigaDNS.app`，或命令行：

```bash
./figadns
```

### 3. 使用 Figma

直接打开 Figma，一切自动加速 🎉

### 4. 验证

```bash
# 查看服务状态
launchctl list | grep figadns

# 测试 DNS 解析
dig @127.0.0.1 -p 5353 figma.com A +short

# 实时日志
tail -f ~/Library/Logs/figadns.log
```

---

## 工作原理

```
┌──────────────────────────────────────────────────┐
│                  你的 Figma 客户端                  │
└────────────┬─────────────────────────────────────┘
             │ 查询 *.figma.com
             ▼
┌──────────────────────┐     ┌──────────────────┐
│   FigaDNS :5353       │────▶│  后台检测引擎      │
│   DNS 服务器            │     │  ┌──────────────┐ │
│                      │     │  │ TCP 延迟       │ │
│   返回当前最优 IP      │     │  │ TLS 握手       │ │
│                      │     │  │ 吞吐量估算      │ │
│                      │     │  │ 丢包率          │ │
│                      │     │  └──────────────┘ │
└──────────────────────┘     └──────────────────┘
         │ 透传非 Figma 域名          ▲
         ▼                            │
┌──────────────────┐          ┌──────────────┐
│ 上游 DNS (114/8.8) │          │  Figma CDN    │
└──────────────────┘          │  节点池        │
                               └──────────────┘
```

**核心特性：**

- **多维评分**：综合 TCP 延迟（30%）、TLS 握手（25%）、吞吐量（30%）、丢包率（15%）
- **自动切换**：当前 IP 评分低于阈值（30分）时，秒级切换次优节点
- **被动检测**：复用真实流量数据，零额外开销
- **零干扰**：仅拦截 `*.figma.com` 域名，其余全部透传

---

## 技术栈

- **语言**：Go
- **DNS 库**：`github.com/miekg/dns`
- **无外部依赖**：编译为独立二进制，macOS 原生运行
- **大小**：~5MB
- **后台服务**：macOS LaunchAgent，关终端不影响

---

## 从源码构建

```bash
git clone https://github.com/MerlinStudio/FigaDNS.git
cd FigaDNS

# Apple Silicon
GOARCH=arm64 go build -ldflags="-s -w" -o figadns .

# Intel Mac
GOARCH=amd64 go build -ldflags="-s -w" -o figadns .

# 通用二进制
GOARCH=arm64 go build -ldflags="-s -w" -o /tmp/figadns-arm64 .
GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/figadns-amd64 .
lipo -create -output figadns /tmp/figadns-arm64 /tmp/figadns-amd64
```

---

## 许可证

MIT License © 2025 Merlin Studio
