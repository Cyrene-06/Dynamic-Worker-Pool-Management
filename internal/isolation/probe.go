package isolation

import (
	"context"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClientProbe 通过读取 RuntimeClass 对象判断某个运行时是否已安装在本集群。
//
// 用 Get 而不是 List：RuntimeClass 数量少且稳定，按名 Get 可以走 controller-runtime
// 的 informer 缓存，开销远低于周期性 List 全部 RuntimeClass。
//
// 注意：这里**不校验** RuntimeClass 的 handler 是否等于对象名。
// 版本化 RuntimeClass（如名为 kata-fc-3-4-0、handler 为 kata-fc）是
// docs/06 §2.1 推荐的升级期共存方案，强制两者同名会把这种正确配置判成不可用。
type ClientProbe struct {
	Reader client.Reader
}

// Exists 实现 RuntimeClassProbe。
func (p *ClientProbe) Exists(ctx context.Context, name string) (bool, error) {
	var rc nodev1.RuntimeClass
	err := p.Reader.Get(ctx, types.NamespacedName{Name: name}, &rc)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
