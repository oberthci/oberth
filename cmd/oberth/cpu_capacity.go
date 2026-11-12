package main

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type cpuCapacity struct {
	mu                    sync.RWMutex
	kube                  kubernetes.Interface
	pool, node            string
	headroom, stepPercent int64
	current               argoworkflow.CPUAdmission
	tolerations           []corev1.Toleration
}

func newCPUCapacity(ctx context.Context, kube kubernetes.Interface, pool, node string, headroom, stepPercent int64, tolerations ...corev1.Toleration) *cpuCapacity {
	c := &cpuCapacity{tolerations: tolerations, kube: kube, pool: pool, node: node, headroom: headroom, stepPercent: stepPercent}
	c.refresh(ctx)
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				c.refresh(ctx)
			}
		}
	}()
	return c
}
func (c *cpuCapacity) refresh(ctx context.Context) {
	p, err := schedulingCPU(ctx, c.kube, c.node, c.headroom, c.stepPercent, c.tolerations...)
	p.Pool = c.pool
	p = p.Canonical()
	if err != nil {
		p.HoldReason = "CPU admission held: node capacity unavailable; verify node read permission"
		log.Printf("CPU capacity refresh unavailable: %v", err)
	}
	c.mu.Lock()
	c.current = p
	c.mu.Unlock()
}
func (c *cpuCapacity) snapshot(context.Context) argoworkflow.CPUAdmission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}
