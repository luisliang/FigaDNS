#!/bin/bash
# FigaDNS 一键安装 — DNS 配置 + 后台服务安装
# 只需执行一次，之后 FigaDNS 开机自启，Terminal 可以安全关闭
set -e

BINARY="$(cd "$(dirname "$0")/../MacOS" && pwd)/figadns"
RESOLVER_FILE="/etc/resolver/figma.com"
LAUNCHD_DIR="$HOME/Library/LaunchAgents"
PLIST_FILE="$LAUNCHD_DIR/com.figadns.daemon.plist"
PORT=5353

echo "============================================"
echo " FigaDNS by Merlin Studio — 一键安装"
echo "============================================"
echo ""
echo "本操作会:"
echo "  ① 配置 DNS 解析器（仅 *.figma.com 走本地）"
echo "  ② 安装开机自启服务"
echo "  ③ 立即启动 FigaDNS"
echo ""
echo "完成后关闭此窗口，FigaDNS 继续在后台运行。"
echo "需要输入管理员密码"
echo ""

# 请求 sudo
sudo -v

# ① 配置 DNS 解析器
echo ""
echo "[1/3] 配置 DNS 解析器..."
sudo mkdir -p /etc/resolver
echo "nameserver 127.0.0.1" | sudo tee "$RESOLVER_FILE" > /dev/null
echo "port $PORT"           | sudo tee -a "$RESOLVER_FILE" > /dev/null
echo "timeout 5"            | sudo tee -a "$RESOLVER_FILE" > /dev/null
echo "  ✅ $RESOLVER_FILE"

# ② 安装 LaunchAgent（用户级后台服务）
echo ""
echo "[2/3] 安装后台服务..."

mkdir -p "$LAUNCHD_DIR"

cat > /tmp/com.figadns.daemon.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
 "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.figadns.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BINARY</string>
        <string>--port=$PORT</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$HOME/Library/Logs/figadns.log</string>
    <key>StandardErrorPath</key>
    <string>$HOME/Library/Logs/figadns.err.log</string>
</dict>
</plist>
EOF

# 卸载旧服务（如果存在）
launchctl unload "$PLIST_FILE" 2>/dev/null || true
cp /tmp/com.figadns.daemon.plist "$PLIST_FILE"

echo "  ✅ $PLIST_FILE"

# ③ 启动服务
echo ""
echo "[3/3] 启动 FigaDNS..."
launchctl load "$PLIST_FILE"
sleep 1

# 验证
if launchctl list | grep -q com.figadns.daemon; then
    echo "  ✅ FigaDNS 服务运行中"
else
    echo "  ⚠️  服务未启动，尝试手动启动..."
    launchctl start com.figadns.daemon 2>/dev/null || true
fi

echo ""
echo "============================================"
echo " 🎉 FigaDNS 安装完成！"
echo "============================================"
echo ""
echo "现在可以:"
echo "  ① 关闭此窗口 — FigaDNS 在后台继续运行"
echo "  ② 打开 Figma — 享受加速"
echo "  ③ 以后想开就开，关就关"
echo ""
echo "📋 管理命令（在 Terminal 中运行）:"
echo "  启动:  launchctl start com.figadns.daemon"
echo "  停止:  launchctl stop  com.figadns.daemon"
echo "  状态:  launchctl list | grep figadns"
echo "  日志:  tail -f ~/Library/Logs/figadns.log"
echo ""
echo "按回车退出..."
read