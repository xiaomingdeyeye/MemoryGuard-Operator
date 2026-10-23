/*
Package controller 实现 MemoryPolicy 的调和（Reconcile）循环。

调和器会 Watch MemoryPolicy 资源，针对每一个策略检查其目标命名空间下的 Pod，
计算每个 Pod 的内存使用率，并为超过配置阈值的 Pod 打标记（label 或 annotation）。
当使用率恢复正常后标记会被移除；通过 finalizer 保证策略被删除时，由它添加的所有标记都会被清理。
*/
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	memoryv1 "github.com/example/memoryguard-operator/api/v1"
)

const (
	// finalizerName 用于保证策略删除时先清理标记。
	finalizerName = "memory.example.com/finalizer"
	// managedByAnnotation 记录某个标记是由哪个策略添加到 Pod 上的，
	// 以便清理时只精确移除该策略自己添加的标记。
	managedByAnnotation = "memoryguard.example.com/policy"
	// requeueInterval 是每个策略重新评估的周期。
	requeueInterval = 30 * time.Second
)

// markedPodsGauge 按策略维度暴露当前被标记的 Pod 数量。
var markedPodsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "memoryguard_marked_pods",
	Help: "Number of Pods currently marked as memory-overloaded, labelled by policy.",
}, []string{"policy"})

func init() {
	metrics.Registry.MustRegister(markedPodsGauge)
}

// MemoryPolicyReconciler 负责调和 MemoryPolicy 对象。
type MemoryPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Metrics 是 metrics.k8s.io 的 clientset。若集群未安装 Metrics API，
	// 该字段可能为 nil，此时调和器会优雅降级。
	Metrics metricsv.Interface
}

// +kubebuilder:rbac:groups=memory.example.com,resources=memorypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=memory.example.com,resources=memorypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=memory.example.com,resources=memorypolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// Reconcile 是单个 MemoryPolicy 的核心调和循环。
func (r *MemoryPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy memoryv1.MemoryPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		// 策略已不存在，无需处理（清理已通过 finalizer 完成）。
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 处理删除：先清理标记，再移除 finalizer。
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutilContains(policy.Finalizers, finalizerName) {
			if err := r.cleanupMarkers(ctx, &policy); err != nil {
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "CleanupFailed",
					"Failed to clean up markers: %v", err)
				return ctrl.Result{}, err
			}
			markedPodsGauge.DeleteLabelValues(policy.Name)
			policy.Finalizers = controllerutilRemove(policy.Finalizers, finalizerName)
			if err := r.Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Event(&policy, corev1.EventTypeNormal, "Cleaned",
				"Removed all markers applied by this policy")
		}
		return ctrl.Result{}, nil
	}

	// 确保存活的策略上都带有 finalizer。
	if !controllerutilContains(policy.Finalizers, finalizerName) {
		policy.Finalizers = append(policy.Finalizers, finalizerName)
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	namespaces := r.targetNamespaces(&policy)
	var observed, marked int32

	for _, ns := range namespaces {
		var pods corev1.PodList
		listOpts := []client.ListOption{}
		if ns != "" {
			listOpts = append(listOpts, client.InNamespace(ns))
		}
		if err := r.List(ctx, &pods, listOpts...); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing pods in %q: %w", ns, err)
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			observed++

			usagePct, ok := r.podMemoryUsagePercent(ctx, pod)
			if !ok {
				// 无法确定该 Pod 的使用率，记录 Event 并跳过。
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "MetricsUnavailable",
					"Unable to determine memory usage for pod %s/%s", pod.Namespace, pod.Name)
				logger.V(1).Info("skipping pod, no usage data",
					"pod", pod.Name, "namespace", pod.Namespace)
				continue
			}

			overloaded := usagePct >= float64(policy.Spec.Threshold)
			isMarked := r.hasMarker(pod, &policy)

			switch {
			case overloaded && !isMarked:
				if err := r.applyMarker(ctx, pod, &policy); err != nil {
					logger.Error(err, "failed to apply marker", "pod", pod.Name)
					continue
				}
				r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "Marked",
					"Pod %s/%s marked (usage %.1f%% >= %d%%)",
					pod.Namespace, pod.Name, usagePct, policy.Spec.Threshold)
				marked++
			case !overloaded && isMarked:
				if err := r.removeMarker(ctx, pod, &policy); err != nil {
					logger.Error(err, "failed to remove marker", "pod", pod.Name)
					continue
				}
				r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "Unmarked",
					"Pod %s/%s unmarked (usage %.1f%% < %d%%)",
					pod.Namespace, pod.Name, usagePct, policy.Spec.Threshold)
			case overloaded && isMarked:
				marked++
			}
		}
	}

	markedPodsGauge.WithLabelValues(policy.Name).Set(float64(marked))

	now := metav1.Now()
	policy.Status.MarkedPods = marked
	policy.Status.ObservedPods = observed
	policy.Status.LastReconcileTime = &now
	meta := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("Observed %d pods, %d marked", observed, marked),
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}
	setCondition(&policy.Status.Conditions, meta)
	if err := r.Status().Update(ctx, &policy); err != nil {
		logger.Error(err, "failed to update status")
	}

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// targetNamespaces 解析一个策略应监控的命名空间列表。
// 若只返回一个空字符串元素（""），表示“所有命名空间”。
func (r *MemoryPolicyReconciler) targetNamespaces(p *memoryv1.MemoryPolicy) []string {
	if len(p.Spec.Namespaces) > 0 {
		return p.Spec.Namespaces
	}
	if p.Spec.Namespace != "" {
		return []string{p.Spec.Namespace}
	}
	return []string{""} // 所有命名空间
}

// podMemoryUsagePercent 返回 Pod 内存使用量占其内存 limit 的百分比。
// 优先使用 Metrics API；若不可用，则优雅降级为基于 Pod 的 requests 与 limits 的估算值。
func (r *MemoryPolicyReconciler) podMemoryUsagePercent(ctx context.Context, pod *corev1.Pod) (float64, bool) {
	limit := podMemoryLimit(pod)

	if r.Metrics != nil {
		pm, err := r.Metrics.MetricsV1beta1().PodMetricses(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err == nil {
			var used int64
			for _, c := range pm.Containers {
				used += c.Usage.Memory().Value()
			}
			if limit > 0 {
				return float64(used) / float64(limit) * 100, true
			}
			// 未设置 limit：退而以 requests 作为分母。
			if reqs := podMemoryRequests(pod); reqs > 0 {
				return float64(used) / float64(reqs) * 100, true
			}
		}
	}

	// 优雅降级：Metrics API 不可用，用 requests/limits 比值估算内存压力
	//（requests 近似代表稳态使用量）。
	reqs := podMemoryRequests(pod)
	if limit > 0 && reqs > 0 {
		return float64(reqs) / float64(limit) * 100, true
	}
	return 0, false
}

// podMemoryLimit 累加 Pod 中所有容器的内存 limit。
func podMemoryLimit(pod *corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			total += q.Value()
		}
	}
	return total
}

// podMemoryRequests 累加 Pod 中所有容器的内存 requests。
func podMemoryRequests(pod *corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			total += q.Value()
		}
	}
	return total
}

// hasMarker 判断 Pod 当前是否带有该策略的标记。
func (r *MemoryPolicyReconciler) hasMarker(pod *corev1.Pod, p *memoryv1.MemoryPolicy) bool {
	if pod.Annotations[managedByAnnotation] != p.Name {
		return false
	}
	switch p.Spec.Action {
	case memoryv1.ActionAddLabel:
		return pod.Labels[p.Spec.Marker.Key] == p.Spec.Marker.Value
	case memoryv1.ActionAddAnnotation:
		return pod.Annotations[p.Spec.Marker.Key] == p.Spec.Marker.Value
	}
	return false
}

// applyMarker 为 Pod 添加策略的 label/annotation，以及一个归属 annotation。
func (r *MemoryPolicyReconciler) applyMarker(ctx context.Context, pod *corev1.Pod, p *memoryv1.MemoryPolicy) error {
	patched := pod.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[managedByAnnotation] = p.Name

	switch p.Spec.Action {
	case memoryv1.ActionAddLabel:
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[p.Spec.Marker.Key] = p.Spec.Marker.Value
	case memoryv1.ActionAddAnnotation:
		patched.Annotations[p.Spec.Marker.Key] = p.Spec.Marker.Value
	}
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// removeMarker 删除策略的 label/annotation 以及归属 annotation。
func (r *MemoryPolicyReconciler) removeMarker(ctx context.Context, pod *corev1.Pod, p *memoryv1.MemoryPolicy) error {
	patched := pod.DeepCopy()
	switch p.Spec.Action {
	case memoryv1.ActionAddLabel:
		delete(patched.Labels, p.Spec.Marker.Key)
	case memoryv1.ActionAddAnnotation:
		delete(patched.Annotations, p.Spec.Marker.Key)
	}
	delete(patched.Annotations, managedByAnnotation)
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

// cleanupMarkers 移除该策略在其所有目标命名空间中添加过的全部标记。
func (r *MemoryPolicyReconciler) cleanupMarkers(ctx context.Context, p *memoryv1.MemoryPolicy) error {
	for _, ns := range r.targetNamespaces(p) {
		var pods corev1.PodList
		opts := []client.ListOption{}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := r.List(ctx, &pods, opts...); err != nil {
			return err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Annotations[managedByAnnotation] != p.Name {
				continue
			}
			if err := r.removeMarker(ctx, pod, p); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// setCondition 按 type 插入或更新一个 condition。
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i := range *conds {
		if (*conds)[i].Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// controllerutilContains 判断切片 s 是否包含元素 v。
func controllerutilContains(s []string, v string) bool {
	for _, item := range s {
		if item == v {
			return true
		}
	}
	return false
}

// controllerutilRemove 返回从 s 中移除所有 v 之后的切片。
func controllerutilRemove(s []string, v string) []string {
	out := s[:0]
	for _, item := range s {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

// SetupWithManager 将调和器注册到 manager，并 Watch MemoryPolicy 资源。
func (r *MemoryPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&memoryv1.MemoryPolicy{}).
		Named("memorypolicy").
		Complete(r)
}
