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
build: ## 编译全部包
	go build ./...

.PHONY: test
test: ## 单元测试（纯函数，不需要集群与 KVM）
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
test-envtest: envtest-assets ## 跑需要真实 API Server 的测试（CRD 校验、状态机集成）
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)/k8s -p path)" \
		go test ./internal/... -count=1 -tags envtest

##@ 本地集群

.PHONY: kind-up
kind-up: ## 创建本地 kind 集群
	kind create cluster --config hack/kind-config.yaml

.PHONY: kind-down
kind-down: ## 删除本地 kind 集群
	kind delete cluster --name sandbox-dev

.PHONY: install
install: manifests ## 安装 CRD
	kubectl apply -k config/crd

.PHONY: deploy-local
deploy-local: install ## 部署到本地集群（simulated 隔离）
	kubectl apply -k config/isolation
	kubectl apply -k config/samples
	go run ./cmd/operator --isolation-config=config/isolation/levels.yaml --leader-elect=false

.PHONY: undeploy
undeploy: ## 卸载示例资源（保留 CRD）
	-kubectl delete -k config/samples --ignore-not-found
	-kubectl delete -k config/isolation --ignore-not-found

##@ 清理

.PHONY: clean
clean: ## 清理构建产物
	rm -rf bin cover.out
