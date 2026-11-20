# IPTV-ZT-Bridge Makefile
# 输出两个独立套件：Server（内置 rtp2httpd） / Client

APP_NAME    := iptv-zt-bridge
VERSION     := 0.2.25
GIT_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

ARCHS           := x86_64 armv7 arm64
GOARCH_x86_64   := amd64
GOARCH_armv7    := arm
GOARCH_arm64    := arm64

ZEROTIER_VERSION := 1.14.2
RTP2HTTPD_VERSION := 3.9.1

# rtp2httpd 官方静态二进制品命名
RTP_ASSET_x86_64 := x86_64
RTP_ASSET_arm64  := aarch64
RTP_ASSET_armv7  := armv7-eabihf

BUILD_DIR := build
DIST_DIR  := dist

LDFLAGS := -s -w \
	-X main.Version=$(VERSION) \
	-X main.GitCommit=$(GIT_COMMIT) \
	-X main.BuildTime=$(BUILD_TIME)

.PHONY: all build build-all clean spk spk-server spk-client deb-arm64 \
	download-zerotier download-rtp2httpd test lint help

help: ## 显示帮助
	@echo "IPTV-ZT-Bridge Build"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'

all: build-all ## 编译所有架构

build: ## 编译当前架构（默认 x86_64）
	@$(MAKE) build-x86_64

build-all: build-x86_64 build-armv7 build-arm64 ## 编译所有架构

build-x86_64:
	@echo "[build] x86_64"
	@mkdir -p $(BUILD_DIR)/x86_64
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_x86_64) \
		go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/x86_64/$(APP_NAME) ./cmd/iptv-zt-bridge

build-armv7:
	@echo "[build] armv7"
	@mkdir -p $(BUILD_DIR)/armv7
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_armv7) GOARM=7 \
		go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/armv7/$(APP_NAME) ./cmd/iptv-zt-bridge

build-arm64:
	@echo "[build] arm64"
	@mkdir -p $(BUILD_DIR)/arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_arm64) \
		go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/arm64/$(APP_NAME) ./cmd/iptv-zt-bridge

download-zerotier: download-zerotier-x86_64 download-zerotier-armv7 download-zerotier-arm64 ## 下载 ZeroTier 1.14.2

download-zerotier-x86_64:
	@bash spk/fetch-zerotier.sh x86_64

download-zerotier-armv7:
	@bash spk/fetch-zerotier.sh armv7

download-zerotier-arm64:
	@bash spk/fetch-zerotier.sh arm64

download-rtp2httpd: download-rtp2httpd-x86_64 download-rtp2httpd-armv7 download-rtp2httpd-arm64 ## 下载 rtp2httpd 静态二进制

download-rtp2httpd-x86_64:
	@mkdir -p $(BUILD_DIR)/x86_64/bin
	curl -fsSL -o $(BUILD_DIR)/x86_64/bin/rtp2httpd \
		"https://github.com/stackia/rtp2httpd/releases/download/v$(RTP2HTTPD_VERSION)/rtp2httpd-$(RTP2HTTPD_VERSION)-$(RTP_ASSET_x86_64)"
	@chmod +x $(BUILD_DIR)/x86_64/bin/rtp2httpd

download-rtp2httpd-armv7:
	@mkdir -p $(BUILD_DIR)/armv7/bin
	curl -fsSL -o $(BUILD_DIR)/armv7/bin/rtp2httpd \
		"https://github.com/stackia/rtp2httpd/releases/download/v$(RTP2HTTPD_VERSION)/rtp2httpd-$(RTP2HTTPD_VERSION)-$(RTP_ASSET_armv7)"
	@chmod +x $(BUILD_DIR)/armv7/bin/rtp2httpd

download-rtp2httpd-arm64:
	@mkdir -p $(BUILD_DIR)/arm64/bin
	curl -fsSL -o $(BUILD_DIR)/arm64/bin/rtp2httpd \
		"https://github.com/stackia/rtp2httpd/releases/download/v$(RTP2HTTPD_VERSION)/rtp2httpd-$(RTP2HTTPD_VERSION)-$(RTP_ASSET_arm64)"
	@chmod +x $(BUILD_DIR)/arm64/bin/rtp2httpd

spk: spk-server spk-client ## 打包 Server + Client 两个套件

spk-server: build-all download-zerotier download-rtp2httpd ## 打包服务端套件
	@mkdir -p $(DIST_DIR)
	@for a in $(ARCHS); do bash spk/build-spk.sh $$a server $(VERSION) || exit 1; done

spk-client: build-all download-zerotier ## 打包客户端套件
	@mkdir -p $(DIST_DIR)
	@for a in $(ARCHS); do bash spk/build-spk.sh $$a client $(VERSION) || exit 1; done

deb-arm64: build-arm64 download-zerotier-arm64 download-rtp2httpd-arm64 ## 打包飞牛/Debian arm64 安装包
	@mkdir -p $(DIST_DIR)
	@for k in server client; do bash deb/build-deb.sh arm64 $$k $(VERSION) || exit 1; done

fpk-arm64: build-arm64 download-zerotier-arm64 download-rtp2httpd-arm64 ## 打包飞牛应用中心 arm64 包
	@mkdir -p $(DIST_DIR)
	@for k in server client; do bash deb/build-fpk.sh arm64 $$k $(VERSION) || exit 1; done

test: ## 运行测试
	go test ./...

lint: ## 代码检查
	go vet ./...

clean: ## 清理构建产物
	rm -rf $(BUILD_DIR) $(DIST_DIR)
