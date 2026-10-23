// Package v1 包含 memory v1 API 组的 API Schema 定义。
// +kubebuilder:object:generate=true
// +groupName=memory.example.com
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion 是用于注册这些对象的 group version。
	GroupVersion = schema.GroupVersion{Group: "memory.example.com", Version: "v1"}

	// SchemeBuilder 用于把 go 类型注册到 GroupVersionKind scheme。
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme 把本 group-version 下的类型添加到给定的 scheme。
	AddToScheme = SchemeBuilder.AddToScheme
)
