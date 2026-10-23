# MemoryGuard Operator

Kubernetes Operator：自动监控 Pod 内存使用率。超过 `MemoryPolicy` 配置的阈值时给 Pod 打 label 或 annotation；使用率恢复后移除标记；策略删除时清理由它添加的全部标记。可用于对接 HPA、监控告警等下游系统。

基于 [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)，采用 Kubebuilder 风格目录（手工搭建，构建不依赖 kubebuilder / Operator SDK 二进制）。

## 功能

- 自定义资源 `MemoryPolicy`（集群级）：阈值、动作、标记键值
- 调和循环：列出目标 Pod → 计算内存使用率 → 打标 / 去标
- 优先通过 Metrics API（`metrics.k8s.io`）取真实用量；不可用时**优雅降级**为 requests/limits 估算
- Finalizer 保证删除策略时清理该策略加过的标记
- 打标、去标、指标失败、清理均记录 Kubernetes Event
- `spec.namespaces` 支持多命名空间
- Prometheus 指标 `memoryguard_marked_pods{policy=...}`，默认监听 `:8080`
- CRD OpenAPI 校验：`threshold` 0–100，`action` 枚举

## 项目结构

```
memoryguard-operator/
├── api/v1/                          # MemoryPolicy 类型定义与 deepcopy
│   ├── groupversion_info.go
│   ├── memorypolicy_types.go
│   └── zz_generated.deepcopy.go
├── cmd/main.go                      # 入口：注册 scheme、Metrics 客户端、启动 manager
├── internal/controller/             # 调和逻辑与单元测试
│   ├── memorypolicy_controller.go
│   └── memorypolicy_controller_test.go
├── config/
│   ├── crd/bases/                   # CRD 清单
│   ├── rbac/                        # ServiceAccount、ClusterRole、绑定
│   ├── manager/                     # 命名空间 + Deployment
│   └── samples/                     # 示例 MemoryPolicy
├── test/stress-pod.yaml             # 用 stress 制造内存压力的测试 Pod
├── Dockerfile
├── Makefile
└── go.mod
```

## 工作原理

对每个 `MemoryPolicy`，Controller 会：

1. 解析目标命名空间（`spec.namespaces` → `spec.namespace` → 全部命名空间）
2. 列出这些命名空间中 Phase 为 Running 的 Pod
3. 计算每个 Pod 的内存使用率：
   - Metrics API 可用：`used / memoryLimit * 100`（未设 limit 时退化为 `used / memoryRequests`）
   - Metrics API 不可用（降级）：`requests / limit * 100` 作为近似
4. 使用率 ≥ 阈值且未标记 → 打标；使用率 < 阈值且已标记 → 去标
5. 把 marked/observed 写入 `.status`，更新 Prometheus 指标，30 秒后再次入队

归属通过 annotation `memoryguard.example.com/policy=<策略名>` 记录，清理时只移除该策略自己加的标记。

## 前置条件

- 可用的 Kubernetes 集群（kind / minikube / Docker Desktop 均可）
- 已配置 `kubectl`
- 本地构建需要 Go 1.23+
- 要拿到**真实**内存用量需安装 [metrics-server](https://github.com/kubernetes-sigs/metrics-server)；没有它 Operator 仍可在降级模式下运行

## 如何部署

### 方式 A：本机对着集群跑（最快，适合本地测试）

```bash
# 安装 CRD
make install            # kubectl apply -f config/crd/bases

# 用当前 kubeconfig 在本机启动 Controller
make run                # go run ./cmd/main.go
```

kind 集群示例：

```bash
kind create cluster --name memoryguard --image kindest/node:v1.31.0 --wait 180s
kubectl config use-context kind-memoryguard
make install
make run
```

kind 上安装 metrics-server 时需加上 `--kubelet-insecure-tls`，并把镜像 load 进节点（节点默认拉不到 Docker Hub）：

```bash
# 打了 --kubelet-insecure-tls 的清单见上游 components.yaml
kind load docker-image registry.k8s.io/metrics-server/metrics-server:v0.7.2 --name memoryguard
kubectl -n kube-system rollout status deploy/metrics-server
```

### 方式 B：部署进集群

```bash
make docker-build IMG=memoryguard-operator:latest
kind load docker-image memoryguard-operator:latest --name memoryguard

make deploy
kubectl -n memoryguard-system get pods
```

## 如何测试

### 端到端（作业要求的三个场景）

```bash
# 1. 创建策略：default 命名空间内使用率 >80% 的 Pod 打 label
kubectl apply -f - <<'EOF'
apiVersion: memory.example.com/v1
kind: MemoryPolicy
metadata:
  name: example-policy
spec:
  namespace: "default"
  threshold: 80
  action: "add-label"
  marker:
    key: "memory-overload"
    value: "true"
EOF
kubectl get memorypolicy            # 简写：kubectl get mp

# 2. 创建压测 Pod（limit 100Mi，stress 约 90Mi，超过 80%）
# 若节点拉不到 Docker Hub，先 kind load docker-image polinux/stress:latest
kubectl apply -f test/stress-pod.yaml
kubectl wait --for=condition=Ready pod/memory-hog --timeout=60s

# 3. 超阈值后应被打标（调和周期约 30s，metrics 采集也需几秒）
kubectl get pod memory-hog --show-labels
#   期望：labels 含 memory-overload=true
kubectl describe memorypolicy example-policy
#   Event：Pod default/memory-hog marked (usage xx% >= 80%)

# 4. 恢复：把阈值临时调到 99，用量不再超标，标记应被移除
kubectl patch memorypolicy example-policy --type merge -p '{"spec":{"threshold":99}}'
# 等待约 30s
kubectl get pod memory-hog --show-labels
#   期望：memory-overload 已去掉
#   Event：Pod default/memory-hog unmarked (usage xx% < 99%)

# 5. 删除策略：先改回 80 重新打标，再删除，标记应被清理
kubectl patch memorypolicy example-policy --type merge -p '{"spec":{"threshold":80}}'
# 等待重新打标后：
kubectl delete memorypolicy example-policy
kubectl get pod memory-hog --show-labels
#   期望：只剩 app=memory-hog，策略加的 label / 归属 annotation 都已清除
```

本地 kind 实测结果（`memory-hog` 约 94Mi / 100Mi ≈ 94.9%）。完整命令、期望结果和截图见 [stepDocs/测试步骤与截图.md](stepDocs/测试步骤与截图.md)。

| 场景 | 结果 |
| --- | --- |
| 超阈值打标 | `memory-overload=true`，Event：`marked (usage 94.8% >= 80%)` |
| 恢复后去标 | 阈值改为 99 后 label 消失，Event：`unmarked (usage 94.8% < 99%)` |
| 删除策略清理 | `kubectl delete memorypolicy` 后 label 与归属 annotation 均清除 |

查看 Prometheus 指标：

```bash
# 本机运行时直接访问
curl -s localhost:8080/metrics | grep memoryguard_marked_pods

# 集群内 Deployment 则 port-forward
kubectl -n memoryguard-system port-forward deploy/memoryguard-controller-manager 8080:8080
```

### 单元测试

```bash
make test        # go test ./...
```

使用 controller-runtime 的 fake client，完全离线（降级模式），覆盖：超阈值打标 / 未超不打标、策略删除时 finalizer 清理标记。

## MemoryPolicy 字段说明

```yaml
apiVersion: memory.example.com/v1
kind: MemoryPolicy
metadata:
  name: example-policy
spec:
  namespace: "default"      # 单个命名空间（可选）
  namespaces: ["a", "b"]    # 或多个命名空间（非空时优先于 namespace）
  threshold: 80             # 0-100，占内存 limit 的百分比
  action: "add-label"       # add-label | add-annotation
  marker:
    key: "memory-overload"
    value: "true"
```

`namespace` 与 `namespaces` 都为空时，监控**全部**命名空间。

## 遇到的挑战和解决方案

- **使用率百分比没有统一分母。** 真实用量来自 Metrics API，但百分比需要基准。实现上用 Pod 各容器内存 **limit 之和** 作分母；未设 limit 时退化为 **requests**。
- **集群可能没装 Metrics API。** 不直接失败，而是降级：用 requests/limits 比值估算压力，并打 `MetricsUnavailable` Event，缺口可见。
- **清理时只能动自己加的标记。** 仅靠 marker 的 key/value 无法判断归属。打标时同时写归属 annotation `memoryguard.example.com/policy`，finalizer 按它精确清理。
- **离线无法跑 controller-gen。** `zz_generated.deepcopy.go` 和 CRD 清单按生成器输出格式手工编写。
- **本地网络拉不动 Docker Hub。** kind 节点镜像、metrics-server、stress 均改从国内镜像源（如 `docker.1ms.run`）拉取，再 `docker tag` + `kind load` 进节点；stress Pod 使用 `imagePullPolicy: IfNotPresent`，避免 kubelet 再次访问 Docker Hub。kind 的 metrics-server 还需 `--kubelet-insecure-tls`。
- **status 更新偶发冲突。** 第一次调和会先 `Update` 写入 finalizer，紧接着 `Status().Update` 可能遇到 resourceVersion 冲突。下一次调和会自动重试，不影响最终一致性。

## 可选：重新生成 CRD / deepcopy

安装 Kubebuilder 工具链后可重新生成，不必手改：

```bash
controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."
controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
```
