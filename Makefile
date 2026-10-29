VERSION ?= 0.5.1
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# 交叉编译强制关闭 CGO：纯 Go 实现无需目标平台 C 工具链，
# 产物为静态链接二进制，可在任意 Linux 发行版直接运行。
CGO_ENABLED ?= 0

LDFLAGS := -s -w \
	-X github.com/imex/cmd.Version=$(VERSION) \
	-X github.com/imex/cmd.BuildTime=$(BUILD_TIME) \
	-X github.com/imex/cmd.GitCommit=$(GIT_COMMIT)

GOBUILD := CGO_ENABLED=$(CGO_ENABLED) go build -trimpath -ldflags "$(LDFLAGS)"

BINARY := imex
BUILD_DIR := bin

# ---- UPX 压缩（快速模式）----
# UPX_LEVEL=-1 为 upx 的快速压缩档位（最省时）；--best/-9 为最高压缩率但最慢。
# COMPRESS=1 时构建完成后自动用 upx 压缩产物；未安装 upx 则跳过且不报错。
UPX ?= upx
UPX_LEVEL ?= -1
UPX_BIN := $(shell command -v $(UPX) 2>/dev/null)
COMPRESS ?= 0

# 构建后自动压缩的钩子：仅当 COMPRESS=1 且检测到 upx 时执行，否则静默跳过。
maybe_compress = @if [ "$(COMPRESS)" = "1" ] && [ -n "$(UPX_BIN)" ]; then \
	for f in $(BUILD_DIR)/$(BINARY) $(BUILD_DIR)/$(BINARY)-linux-* $(BUILD_DIR)/$(BINARY)-windows-*; do \
		[ -f "$$f" ] && $(UPX) $(UPX_LEVEL) -q "$$f" || true; \
	done; \
	echo "upx compressed (fast mode, level=$(UPX_LEVEL))"; \
elif [ "$(COMPRESS)" = "1" ] && [ -z "$(UPX_BIN)" ]; then \
	echo "COMPRESS=1 but upx not found; skipped"; \
fi

.PHONY: all build build-all linux windows clean docker vet test test-live compress

all: build

build:
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -o $(BUILD_DIR)/$(BINARY) .
	$(maybe_compress)

# 交叉编译 Linux（仅 amd64；其他架构显式指定 GOARCH，如 arm64）
linux:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-amd64 .
	$(maybe_compress)

windows:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-windows-amd64.exe .
	$(maybe_compress)

# 发布平台交叉编译产物（Linux amd64 + Windows amd64；其他平台请在本机 make build 或显式指定 GOARCH）
build-all: linux windows

# 显式压缩：对现有 bin/ 产物单独运行 upx（`make compress`，无需 COMPRESS=1）
compress:
	@if [ -z "$(UPX_BIN)" ]; then \
		echo "upx not found; install upx to enable compression"; \
	else \
		for f in $(BUILD_DIR)/$(BINARY) $(BUILD_DIR)/$(BINARY)-linux-* $(BUILD_DIR)/$(BINARY)-windows-*; do \
			[ -f "$$f" ] && $(UPX) $(UPX_LEVEL) -q "$$f" || true; \
		done; \
		echo "upx compressed (fast mode, level=$(UPX_LEVEL))"; \
	fi

vet:
	CGO_ENABLED=$(CGO_ENABLED) go vet ./...

# -race 需要 cgo，与发布构建的 CGO_ENABLED=0 无关，这里固定为 1。
test:
	CGO_ENABLED=1 go test -race ./...

# 真实 MySQL 端到端测试。需要一个已存在的库：
#   IMEX_TEST_DSN='root:secret@tcp(127.0.0.1:3306)/imex_it' make test-live
test-live:
	@if [ -z "$$IMEX_TEST_DSN" ]; then \
		echo "IMEX_TEST_DSN is not set (e.g. 'root:secret@tcp(127.0.0.1:3306)/imex_it')"; \
		exit 2; \
	fi
	CGO_ENABLED=1 go test -race ./test/integration -run Live -v

clean:
	rm -rf $(BUILD_DIR)

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .
