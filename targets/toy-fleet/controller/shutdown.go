package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const VolumeFinalizer = "fleet.toy/volume"

func Shutdown(ctx context.Context, c client.Client, namespace string, bug Bug) error { return nil }
