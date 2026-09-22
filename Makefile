# ---------------------------------------------------------------------------
# Agent 沙箱控制面 —— 构建与验证入口
#
# 设计原则：所有工具都装进本地 bin/，不依赖开发机的全局环境。
# 这样"能跑 make test"在 CI 与同事机器上是同一套前提，
# 而不是"先按 README 装五个工具、版本还得对得上"。
#
# 注意：Windows 上若没有 make，用 hack/verify.ps1（等价实现，见该脚本头注释）。
# ---------------------------------------------------------------------------

LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_TOOLS_VERSION ?= v0.20.1
ENVTEST_K8S_VERSION ?= 1.32.0

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
SETUP_ENVTEST  ?= $(LOCALBIN)/setup-envtest

# ---- 容器镜像 ----
# 这三个名字是**单一来源**，必须与 config/ 下的清单保持一致
# （config/gateway/deployment.yaml、config/sweeper/cronjob.yaml）。
# 只改这里而忘了改清单，症状是"镜像构建成功、部署上去还是旧版本"——
# 这类问题在滚动升级时最难发现，因为旧 Pod 看起来一切正常。
IMAGE_REGISTRY ?= ghcr.io/cyrene-06
IMAGE_TAG      ?= dev

OPERATOR_IMAGE ?= $(IMAGE_REGISTRY)/dwp-operator:$(IMAGE_TAG)
GATEWAY_IMAGE  ?= $(IMAGE_REGISTRY)/dwp-gateway:$(IMAGE_TAG)
SWEEPER_IMAGE  ?= $(IMAGE_REGISTRY)/dwp-sweeper:$(IMAGE_TAG)

# 多架构构建的目标平台（生产节点混用 x86 与 ARM 时用得上）。
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64

# 模块代理。`?=` 会尊重环境里已有的 GOPROXY，所以本机已配镜像时无需额外传参；
# 想显式覆盖：make docker-build GOPROXY=https://goproxy.cn,direct
# （官方 proxy 不可达时，构建会卡在 go mod download —— 表现为"不报错但一直不动"）
GOPROXY ?= https://proxy.golang.org,direct

DOCKER_BUILD_ARGS ?= --build-arg VERSION=$(IMAGE_TAG) --build-arg GOPROXY=$(GOPROXY)

# kind 集群名，与 hack/kind-config.yaml 的 name 字段一致。
KIND_CLUSTER_NAME ?= sandbox-dev

.PHONY: help
help: ## 显示全部可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

$(LOCALBIN):
	@mkdir -p $(LOCALBIN)

##@ 代码生成

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## 生成 CRD 与 RBAC 清单
	$(CONTROLLER_GEN) crd paths=./api/v1alpha1 \
		output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths=./internal/... \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: $(CONTROLLER_GEN) ## 生成 DeepCopy
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1

$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

##@ 验证

.PHONY: fmt
fmt: ## 格式化代码
	gofmt -w ./api ./cmd ./internal

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 门禁）
	@out=$$(gofmt -l ./api ./cmd ./internal); \
	if [ -n "$$out" ]; then echo "以下文件未通过 gofmt："; echo "$$out"; exit 1; fi; \
	echo "gofmt OK"

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: build
build: ## 编译全部可执行文件（operator / gateway / sweeper）
	go build ./...

.PHONY: manifests-check
manifests-check: ## 校验全部 kustomize 清单可渲染（不需要集群）
	kubectl kustomize config/crd > /dev/null
	kubectl kustomize config/rbac > /dev/null
	kubectl kustomize config/isolation > /dev/null
	kubectl kustomize config/samples > /dev/null
	kubectl kustomize config/manager > /dev/null
	kubectl kustomize config/gateway > /dev/null
	kubectl kustomize config/sweeper > /dev/null
	@echo "all manifests render OK"

.PHONY: test
test: ## 单元测试（纯函数；若已设 KUBEBUILDER_ASSETS 则一并跑 envtest 集成测试，否则自动 skip）
	go test ./... -count=1

.PHONY: cover
cover: ## 单元测试 + 覆盖率
	go test ./... -count=1 -coverprofile=cover.out
	go tool cover -func=cover.out | tail -1

.PHONY: verify
verify: fmt-check vet build test ## 提交前跑这一条就够

##@ envtest（需要下载控制面二进制）

.PHONY: envtest-assets
envtest-assets: $(SETUP_ENVTEST) ## 下载 envtest 的 etcd / kube-apiserver
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)/k8s -p path

.PHONY: test-envtest
test-envtest: envtest-assets ## 跑需要真实 API Server 的测试（CAS 认领并发、CRD 校验）
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)/k8s -p path)" \
		go test ./... -count=1
# 刻意不加 -tags：本项目的 envtest 测试靠 KUBEBUILDER_ASSETS 控制是否 skip，
# 而不是 build tag。加 tag 会产生两组分叉的测试集合，且 `make test` 在资产已就绪时
# 仍然会静默跳过它们 —— 那是“以为覆盖了、其实没跑”的经典成因。

##@ 容器镜像

# 三个镜像出自**同一个** Dockerfile，用 --build-arg BINARY 选择编译哪个 cmd 包
# （理由见 Dockerfile 头注释：避免"升级 Go 版本要改三处"）。
# 需要 BuildKit（Docker 23+ 默认开启），因为用到了 cache mount。
#
# 注意：本机（Windows）当前 Docker 守护进程不可用 —— Docker Desktop 已安装但
# Linux 引擎未初始化。因此这些目标**尚未被真正执行过**，第一次跑之前请先确认
# `docker info` 有输出（README §2.6 有完整说明）。
.PHONY: docker-build
docker-build: ## 构建三个镜像（本地架构：operator / gateway / sweeper）
	docker build $(DOCKER_BUILD_ARGS) --build-arg BINARY=operator -t $(OPERATOR_IMAGE) .
	docker build $(DOCKER_BUILD_ARGS) --build-arg BINARY=gateway  -t $(GATEWAY_IMAGE) .
	docker build $(DOCKER_BUILD_ARGS) --build-arg BINARY=sweeper  -t $(SWEEPER_IMAGE) .

.PHONY: docker-buildx
docker-buildx: ## 多架构构建并直接推送（需要先 docker login）
	docker buildx build --platform $(DOCKER_PLATFORMS) $(DOCKER_BUILD_ARGS) --build-arg BINARY=operator -t $(OPERATOR_IMAGE) --push .
	docker buildx build --platform $(DOCKER_PLATFORMS) $(DOCKER_BUILD_ARGS) --build-arg BINARY=gateway  -t $(GATEWAY_IMAGE) --push .
	docker buildx build --platform $(DOCKER_PLATFORMS) $(DOCKER_BUILD_ARGS) --build-arg BINARY=sweeper  -t $(SWEEPER_IMAGE) --push .

.PHONY: docker-push
docker-push: ## 推送本地架构的三个镜像（需要先 docker login）
	docker push $(OPERATOR_IMAGE)
	docker push $(GATEWAY_IMAGE)
	docker push $(SWEEPER_IMAGE)

# Dockerfile 的静态检查。
#
# 为什么值得单列一个目标：**它不需要容器运行时**。本项目的开发机一度
# 完全没有可用的 Docker（见 README §2.6），那种情况下“镜像能不能构建”
# 几乎无法验证，而语法/规则层面的问题（指令拼错、ARG 作用域不对）
# 完全可以在没有守护进程时就抓出来。
#
# 依赖 hadolint 在 PATH 上（不是本仓库自动安装的 —— 它的发行包按平台命名，
# 加一条下载规则反而多一个会销的网络依赖）：
#   https://github.com/hadolint/hadolint/releases  （二进制改名 hadolint 放入 PATH）
.PHONY: dockerfile-lint
dockerfile-lint: ## 静态检查 Dockerfile（不需要容器运行时；需要 hadolint 在 PATH）
	@command -v hadolint >/dev/null 2>&1 || { echo "hadolint not found on PATH (see the comment above)"; exit 1; }
	hadolint Dockerfile

##@ 本地集群

.PHONY: kind-up
kind-up: ## 创建本地 kind 集群
	kind create cluster --config hack/kind-config.yaml

.PHONY: kind-down
kind-down: ## 删除本地 kind 集群
	kind delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: kind-load
kind-load: ## 把三个镜像加载进 kind 集群（不需要 registry）
	kind load docker-image $(OPERATOR_IMAGE) $(GATEWAY_IMAGE) $(SWEEPER_IMAGE) --name $(KIND_CLUSTER_NAME)

# 一条命令跑完 E1 端到端（docs/10 M1 的退出标准：“以容器方式在 kind 上跑通，
# 且用 Pod 里的 ServiceAccount 而不是开发者的 kubeconfig”）。
#
# 顺序不能变：
#   config/samples 必须先于 config/rbac —— 它负责创建 sandbox-pool / sandbox-system
#   两个命名空间，而 rbac 里有一个位于 sandbox-pool 的 namespaced Role。
#   config/isolation 也必须在 manager 之前 —— 否则控制器会用内置默认配置先跑起来，
#   “集群里哪些隔离级别可用”这个事实就是错的。
#
# rollout status 带 --timeout：失败要响亮，而不是让 CI 一直挂着等一个永远不就绪的 Pod。
# 刻意不在这里 apply gateway/sweeper：前者需要先手工创建 Secret（见 config/gateway）。
#
# 刻意**不**把 kind-up / docker-build / kind-load 写成前置依赖：
# 那样每次都会重新建集群与重建镜像，而这个目标最常被用的场景是
# "镜像已经加载好了，我想重新部署一遍"。前置条件写在注释里，由人决定。
.PHONY: kind-e2e
kind-e2e: ## 把已加载的镜像部署到已存在的 kind 集群并等就绪（前置：kind-up / docker-build / kind-load）
	kubectl apply -k config/crd
	kubectl apply -k config/samples
	kubectl apply -k config/rbac
	kubectl apply -k config/isolation
	kubectl apply -k config/manager
	kubectl -n sandbox-system rollout status deployment/sandbox-operator --timeout=180s

.PHONY: install
install: manifests ## 安装 CRD
	kubectl apply -k config/crd

.PHONY: deploy-local
deploy-local: install ## 部署到本地集群（simulated 隔离）
	kubectl apply -k config/rbac
	kubectl apply -k config/isolation
	kubectl apply -k config/samples
	go run ./cmd/operator --isolation-config=config/isolation/levels.yaml --leader-elect=false

.PHONY: gateway-local
gateway-local: ## 本地运行接入层（需要 SANDBOX_TOKEN_ISSUER_SECRET 环境变量）
	go run ./cmd/gateway --tenant-tokens-file=config/gateway/tenant-tokens.example \
		--sandbox-namespace=sandbox-pool --bind-address=:8090

.PHONY: sweeper-local
sweeper-local: ## 本地干跑一轮对账（只报告不执行）
	go run ./cmd/sweeper --dry-run

.PHONY: deploy-operator
deploy-operator: ## 以容器方式部署控制面（需先 make docker-build 与 make kind-load）
	kubectl apply -k config/rbac
	kubectl apply -k config/isolation
	kubectl apply -k config/manager

.PHONY: deploy-gateway
deploy-gateway: ## 部署接入层（需先手工创建 Secret sandbox-gateway-auth）
	kubectl apply -k config/rbac
	kubectl apply -k config/gateway

.PHONY: deploy-sweeper
deploy-sweeper: ## 部署泄漏对账 CronJob
	kubectl apply -k config/rbac
	kubectl apply -k config/sweeper

.PHONY: undeploy
undeploy: ## 卸载示例资源（保留 CRD）
	-kubectl delete -k config/sweeper --ignore-not-found
	-kubectl delete -k config/gateway --ignore-not-found
	-kubectl delete -k config/manager --ignore-not-found
	-kubectl delete -k config/samples --ignore-not-found
	-kubectl delete -k config/isolation --ignore-not-found

##@ 清理

# 刻意**不**提供 docker-clean：`docker rmi` 会连带删掉与这些镜像共享中间层的
# 其它镜像，而那种破坏是静默的（下一次构建才发现某层得重新下载）。
# 需要清理时请显式写 tag：docker rmi $(GATEWAY_IMAGE)。

.PHONY: clean
clean: ## 清理构建产物
	rm -rf bin cover.out
