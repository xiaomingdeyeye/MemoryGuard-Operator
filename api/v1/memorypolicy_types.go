/*
Package v1 定义 MemoryPolicy 自定义资源。

MemoryPolicy 允许集群运维人员以声明式方式监控 Pod 内存使用情况，并自动为内存使用率
超过配置阈值的 Pod 打标记（label 或 annotation）。当使用率恢复正常后标记会被移除；
当策略自身被删除时，由它创建的所有标记都会被清理。
*/
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ActionType 枚举支持的打标记动作。
// +kubebuilder:validation:Enum=add-label;add-annotation
type ActionType string

const (
	// ActionAddLabel 用 label 标记超载的 Pod。
	ActionAddLabel ActionType = "add-label"
	// ActionAddAnnotation 用 annotation 标记超载的 Pod。
	ActionAddAnnotation ActionType = "add-annotation"
)

// Marker 描述要应用到超载 Pod 上的键值对。
type Marker struct {
	// Key 是要应用的 label/annotation 键。
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Value 是要应用的 label/annotation 值。
	// +kubebuilder:validation:MinLength=1
	Value string `json:"value"`
}

// MemoryPolicySpec 定义 MemoryPolicy 的期望状态。
type MemoryPolicySpec struct {
	// Namespace 是要监控的单个目标命名空间。
	// 若 Namespace 和 Namespaces 都为空，则监控所有命名空间。
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Namespaces 是要监控的目标命名空间列表（进阶：多命名空间支持）。
	// 当非空时优先于 Namespace 生效。
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// Threshold 是内存使用率百分比（0-100），超过该值即视为 Pod 超载。
	// 例如 80 表示 80%。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Threshold int32 `json:"threshold"`

	// Action 决定如何标记一个超载的 Pod。
	// +kubebuilder:validation:Required
	Action ActionType `json:"action"`

	// Marker 是 Pod 超载时应用的键值对。
	// +kubebuilder:validation:Required
	Marker Marker `json:"marker"`
}

// MemoryPolicyStatus 定义 MemoryPolicy 的观测状态。
type MemoryPolicyStatus struct {
	// MarkedPods 是当前被该策略标记的 Pod 数量。
	// +optional
	MarkedPods int32 `json:"markedPods"`

	// ObservedPods 是上一次调和中被评估的 Pod 数量。
	// +optional
	ObservedPods int32 `json:"observedPods"`

	// LastReconcileTime 是上一次成功调和的时间戳。
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// Conditions 表示策略状态的最新观测结果。
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mp
// +kubebuilder:printcolumn:name="Threshold",type=integer,JSONPath=`.spec.threshold`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Marked",type=integer,JSONPath=`.status.markedPods`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryPolicy 是 memorypolicies API 的 Schema。
type MemoryPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MemoryPolicySpec   `json:"spec,omitempty"`
	Status MemoryPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryPolicyList 包含一组 MemoryPolicy。
type MemoryPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MemoryPolicy{}, &MemoryPolicyList{})
}
