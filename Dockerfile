# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Agent 沙箱控制面 —— operator / gateway / sweeper 共用的多阶段构建。
#
# 为什么要一个 Dockerfile 而不是三个：
#   三个镜像的唯一差别是**编译哪个 cmd 包**，基础镜像、构建参数、运行镜像
#   完全相同。拆成三份之后，"升级 Go 版本"这件事就变成改三处 —— 漏掉一处
#   会产生"某个组件还是用旧 Go 编译的"这种只在安全扫描报告里才看得见的差异。
#
# 构建（在仓库根目录执行，Makefile 的 docker-build 已封装）：
#   docker build --build-arg BINARY=operator -t dwp-operator:dev .
#   docker build --build-arg BINARY=gateway  -t dwp-gateway:dev  .
#   docker build --build-arg BINARY=sweeper  -t dwp-sweeper:dev  .
#
# 需要 BuildKit（Docker 23+ 默认开启）：下面的 `--mount=type=cache` 是
# 构建期缓存挂载，经典构建器不支持。
#
# ⚠️ 一个与网络有关、但报错位置极具误导性的失败点：`# syntax=docker/dockerfile:1`
# 会让 BuildKit 先去拉这个前端镜像。拉不到时（受限网络），报错出现在构建
# 刚开始的一瞬，内容里只有那个镜像名 —— 很容易被读成“Dockerfile 写错了”。
# 两个选择：给 daemon 配镜像加速；或者删掉 syntax 行与所有 `--mount=type=cache`
# 参数，回退到经典构建器（能构建，代价是每次全量重编）。
# 把这段写在这里，是因为真实发生时的第一反应通常是去改 Dockerfile 的指令。
#
# 交叉编译策略：构建阶段固定跑在**构建机**架构上（--platform=$BUILDPLATFORM），
# 用 $TARGETOS/$TARGETARCH 交叉产出目标平台二进制。这样多架构构建不需要 QEMU
# 模拟编译 —— 模拟编译慢一个数量级，而且偶发地给出"本地能过、CI 不能过"的差异。
# ---------------------------------------------------------------------------

# 与 go.mod 的 `go` 指令（1.27.1）保持一致。
# 注意：升级 Go 时这里与 go.mod 必须同一次改动，否则会出现
# "本地 go build 能过、镜像里编译失败"（新语法 / 新标准库 API）。
ARG GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

# 要编译哪个 cmd 包。**刻意不给默认值**：
# 给了默认值之后，忘记传参的 `docker build .` 会静默产出一个 operator 镜像，
# 而那个镜像被拿去当 gateway 用时才会失败 —— 故障出现在部署阶段，
# 离出错的地方太远。宁可让构建当场报错。
ARG BINARY

# 目标平台，由 BuildKit 根据 --platform 自动注入。
ARG TARGETOS
ARG TARGETARCH

# 模块代理。默认走官方代理；在代理不可达的网络里显式覆盖，否则构建会
# 卡在下面的 `go mod download`（表现为"构建没报错但一直不动"）：
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct ...
# 刻意把默认值写出来而不是留空：依赖究竟从哪儿拉的，本身就是一个需要
# 能被回答的问题（供应链审计），不该隐含在构建机的环境变量里。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 依赖单独一层：只要 go.mod / go.sum 没变，改业务代码不会重新下载模块。
# 在 CI 里这是几分钟与几秒的差别。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0：产出静态链接的二进制。
# 这不是顺手加的优化，而是运行阶段能用 distroless/static 的前提 ——
# 带 cgo 的二进制需要 glibc，基础镜像就得换成 debian（体积 ×10）。
# 本项目全部依赖都是纯 Go（client-go 体系本身纯 Go），关掉 cgo 没有代价。
#
# -trimpath：去掉二进制里的构建机绝对路径。它既泄漏内部目录结构
# （panic 栈会直接打出来），也让不同机器构建的产物字节不一致 ——
# 而字节一致是"镜像可复现"与"签名校验"的基础。
#
# -s -w：去掉符号表与 DWARF 调试信息。代价是无法用 dlv 直接调试镜像内进程；
# 需要调试时请用本机 `go run`，而不是把调试信息塞进生产镜像。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${BINARY}

# ---------------------------------------------------------------------------
# 运行阶段
#
# 为什么是 distroless/static 而不是 scratch：
#   scratch 里没有 CA 证书包。控制面的**唯一**外部依赖就是 API Server，
#   而它是 HTTPS —— 缺 CA 包会以
#   "x509: certificate signed by unknown authority" 的形式炸在启动时，
#   报错完全指不到真正的原因（缺文件）。distroless/static 带 CA 包、
#   tzdata 与 /etc/passwd，同时没有 shell、没有包管理器。
#
# 为什么不是 alpine：
#   musl 与 glibc 的 DNS 解析路径不同（Go 在 CGO_ENABLED=0 下用自己的解析器，
#   但镜像里其它工具不是），且 alpine 里一条 `apk add` 就能把 shell 装回来。
#   多 2MB 换掉一整类"本地与集群行为不一致"，划算。
#
# 为什么用 nonroot 标签：
#   它是 UID/GID 65532，与 config/ 下两份工作负载清单
#   （gateway Deployment、sweeper CronJob）里的
#   `securityContext.runAsUser: 65532` 是同一个值。镜像与清单对不上时，
#   Pod 会在 readOnlyRootFilesystem 下以 root 身份启动失败 —— 报错很难读。
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=0.0.0-dev

# OCI 标签。org.opencontainers.image.source 尤其重要：
# GHCR 用它把镜像包关联到仓库，没有它推送出来的包在 GitHub 上是"无主"的，
# 可见性与权限都得手工维护。
LABEL org.opencontainers.image.source="https://github.com/Cyrene-06/Dynamic-Worker-Pool-Management" \
      org.opencontainers.image.title="Dynamic Worker Pool Management" \
      org.opencontainers.image.description="Kubernetes control plane for agent sandboxes (operator / gateway / sweeper)" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=builder /out/app /app

# 三个组件共用同一个入口路径：镜像的差异在**构建期**就已确定
# （编译的是哪个 cmd 包），运行期不需要、也不应该再用环境变量切换组件。
#
# 前置条件：非 root、只读根文件系统、不需要任何 Linux capabilities
# —— 这一点由 config/ 下两份工作负载清单的 securityContext 声明，镜像本身不做约束，
# 因为约束在清单里才能被集群审计到（镜像里的声明对 K8s 不可见）。
ENTRYPOINT ["/app"]
