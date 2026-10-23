package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	memoryv1 "github.com/example/memoryguard-operator/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := memoryv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// pod 构造一个处于 Running 状态的 Pod，内存 requests/limits 单位为 Mi。
func pod(name string, reqMi, limMi int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(miStr(reqMi))},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(miStr(limMi))},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func miStr(mi int64) string { return resource.NewQuantity(mi*1024*1024, resource.BinarySI).String() }

// TestReconcileMarksOverloadedPods 验证在降级模式（无 Metrics API）下，
// requests/limits 比值超过阈值的 Pod 会被标记，低于阈值的则不会。
func TestReconcileMarksOverloadedPods(t *testing.T) {
	s := testScheme(t)
	policy := &memoryv1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: memoryv1.MemoryPolicySpec{
			Namespace: "default",
			Threshold: 80,
			Action:    memoryv1.ActionAddLabel,
			Marker:    memoryv1.Marker{Key: "memory-overload", Value: "true"},
		},
	}
	hot := pod("hot", 90, 100)   // 90% -> 被标记
	cold := pod("cold", 10, 100) // 10% -> 不标记

	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(policy, hot, cold).
		WithStatusSubresource(policy).Build()

	r := &MemoryPolicyReconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(100)}

	// 第一次调和添加 finalizer；第二次执行打标记。
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	got := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "hot"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["memory-overload"] != "true" {
		t.Errorf("expected hot pod marked, labels=%v", got.Labels)
	}

	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "cold"}, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Labels["memory-overload"]; ok {
		t.Errorf("expected cold pod unmarked, labels=%v", got.Labels)
	}
}

// TestCleanupOnDelete 验证策略被删除时 finalizer 会移除标记。
func TestCleanupOnDelete(t *testing.T) {
	s := testScheme(t)
	now := metav1.Now()
	policy := &memoryv1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: memoryv1.MemoryPolicySpec{
			Namespace: "default",
			Threshold: 80,
			Action:    memoryv1.ActionAddLabel,
			Marker:    memoryv1.Marker{Key: "memory-overload", Value: "true"},
		},
	}
	marked := pod("hot", 90, 100)
	marked.Labels = map[string]string{"memory-overload": "true"}
	marked.Annotations = map[string]string{managedByAnnotation: "p"}

	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(policy, marked).
		WithStatusSubresource(policy).Build()

	r := &MemoryPolicyReconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(100)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "hot"}, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Labels["memory-overload"]; ok {
		t.Errorf("expected marker removed after cleanup, labels=%v", got.Labels)
	}
}
