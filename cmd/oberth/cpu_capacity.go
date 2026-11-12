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

const cpuCapacityGrace = 3 * time.Minute

type cpuCapacity struct {
	lastGood              time.Time
	mu                    sync.RWMutex
	kube                  kubernetes.Interface
	pool, node            string
	headroom, stepPercent int64
	current               argoworkflow.CPUAdmission
	tolerations           []corev1.Toleration
	excluded              []string
}

func newCPUCapacity(ctx context.Context, kube kubernetes.Interface, endpoint, node string, excluded []string, headroom, stepPercent int64, tolerations ...corev1.Toleration) *cpuCapacity {
	c := &cpuCapacity{excluded: excluded, tolerations: tolerations, kube: kube, pool: schedulingPool(endpoint, node, excluded), node: node, headroom: headroom, stepPercent: stepPercent}
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
	c.refreshAt(ctx, time.Now())
}

func (c *cpuCapacity) refreshAt(ctx context.Context, now time.Time) {
	p, err := schedulingCPU(ctx, c.kube, c.node, c.excluded, c.headroom, c.stepPercent, c.tolerations...)
	p.Pool = c.pool
	p = p.Canonical()
	if err != nil {
		p.HoldReason = "CPU admission held: node capacity unavailable; verify node read permission"
		log.Printf("CPU capacity refresh unavailable: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !c.lastGood.IsZero() && now.Sub(c.lastGood) < cpuCapacityGrace && c.current.HoldReason == "" {
		return
	}
	if err == nil && p.HoldReason == "" && p.Milli > 0 {
		c.lastGood = now
	}
	c.current = p
}
func (c *cpuCapacity) snapshot(context.Context) argoworkflow.CPUAdmission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}
