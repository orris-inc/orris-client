#!/bin/bash
set -e

error() { echo -e "\033[31m\033[01m$*\033[0m" && exit 1; }
info() { echo -e "\033[32m\033[01m$*\033[0m"; }
hint() { echo -e "\033[33m\033[01m$*\033[0m"; }

SERVICE_NAME="orris-agent"
INSTALL_DIR="/opt/${SERVICE_NAME}"

if [ -z "$DOWNLOAD_HOST" ]; then
	DOWNLOAD_HOST="https://github.com/orris-inc/orris-client/releases/latest/download"
fi

# 解析参数
TOKEN=""
SERVER_URL=""
while getopts "t:u:" opt; do
	case $opt in
	t) TOKEN="$OPTARG" ;;
	u) SERVER_URL="$OPTARG" ;;
	*) ;;
	esac
done

# 卸载
if [ "$1" == "uninstall" ]; then
	hint "正在卸载 ${SERVICE_NAME}..."
	systemctl disable --now "${SERVICE_NAME}" 2>/dev/null || true
	rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
	rm -rf "${INSTALL_DIR}"
	info "卸载完成"
	exit 0
fi

# 检查参数
if [ -z "$TOKEN" ]; then
	error "缺少参数 -t TOKEN"
fi

# 检查 root
if [ "$EUID" -ne 0 ]; then
	error "请使用 root 权限运行"
fi

# 检测架构
case $(uname -m) in
aarch64 | arm64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*) error "不支持的 CPU 架构: $(uname -m)" ;;
esac

info "检测到架构: linux/${ARCH}"

# 检查是否已安装
if [ -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
	hint "服务已存在，将进行更新..."
	systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
fi

# 创建目录
mkdir -p "${INSTALL_DIR}"
cd "${INSTALL_DIR}"

# 下载或复制二进制
if [ -n "$LOCAL_BINARY" ]; then
	cp "$LOCAL_BINARY" "${INSTALL_DIR}/${SERVICE_NAME}"
else
	DOWNLOAD_URL="${DOWNLOAD_HOST}/${SERVICE_NAME}-linux-${ARCH}"
	info "正在下载: ${DOWNLOAD_URL}"
	curl -fLSs -o "${SERVICE_NAME}" "${DOWNLOAD_URL}" || error "下载失败"
fi

chmod +x "${SERVICE_NAME}"

# 构建启动参数
EXEC_ARGS="--token ${TOKEN}"
if [ -n "$SERVER_URL" ]; then
	EXEC_ARGS="${EXEC_ARGS} --server ${SERVER_URL}"
fi

# 创建 systemd 服务
cat >/etc/systemd/system/${SERVICE_NAME}.service <<EOF
[Unit]
Description=Orris Forward Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Restart=always
RestartSec=5
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/${SERVICE_NAME} ${EXEC_ARGS}

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

info "安装成功"
echo
echo "查看状态: systemctl status ${SERVICE_NAME}"
echo "查看日志: journalctl -u ${SERVICE_NAME} -f"
echo
info "如需卸载，请运行："
echo "systemctl disable --now ${SERVICE_NAME} ; rm -f /etc/systemd/system/${SERVICE_NAME}.service ; rm -rf ${INSTALL_DIR}"
