// Package v1alpha1 定义 sandbox.example.com API 组的 v1alpha1 版本。
// +kubebuilder:object:generate=true
// +groupName=sandbox.example.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion 是本包注册到 scheme 的 group/version。
	GroupVersion = schema.GroupVersion{Group: "sandbox.example.com", Version: "v1alpha1"}

	// SchemeBuilder 用于把本包的 Kind 注册到 scheme。
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme 把本包全部 Kind 注册进 scheme。
	AddToScheme = SchemeBuilder.AddToScheme
)
