package nodeagent

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// NewImagePuller connects to the node's CRI image service. ImageStatus avoids
// repeatedly pulling images on every reconcile, while PullImage populates the
// same content store used by kubelet.
func NewImagePuller(socket string) (func(context.Context, string) error, func() error, error) {
	if socket == "" {
		return nil, nil, fmt.Errorf("CRI socket is required")
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	service := runtimev1.NewImageServiceClient(conn)
	pull := func(ctx context.Context, image string) error {
		spec := &runtimev1.ImageSpec{Image: image}
		status, err := service.ImageStatus(ctx, &runtimev1.ImageStatusRequest{Image: spec})
		if err != nil {
			return fmt.Errorf("CRI ImageStatus %s: %w", image, err)
		}
		if status.Image != nil {
			return nil
		}
		if _, err := service.PullImage(ctx, &runtimev1.PullImageRequest{Image: spec}); err != nil {
			return fmt.Errorf("CRI PullImage %s: %w", image, err)
		}
		return nil
	}
	return pull, conn.Close, nil
}
