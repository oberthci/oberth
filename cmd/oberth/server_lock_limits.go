package main

import (
	"context"
	"github.com/oberthci/oberth/internal/service"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
	"strings"
)

func (o *serverOwners) lockLimits(ctx context.Context, locks []service.Lock) {
	limits := map[string]int{}
	maps := map[string]*core.ConfigMap{}
	for i := range locks {
		l := &locks[i]
		if limit, ok := limits[l.Name]; ok {
			l.Limit = limit
			continue
		}
		parts := strings.Split(l.Name, "/")
		limit := 1
		if len(parts) == 4 && parts[1] == "ConfigMap" {
			name := parts[0] + "/" + parts[2]
			cm, read := maps[name]
			if !read {
				var err error
				cm, err = o.cluster.Kube.CoreV1().ConfigMaps(parts[0]).Get(ctx, parts[2], meta.GetOptions{})
				if err != nil {
					cm = nil
				}
				maps[name] = cm
			}
			limit = nativeLockLimit(cm, parts[3])
		}
		l.Limit, limits[l.Name] = limit, limit
	}
}

func nativeLockLimit(cm *core.ConfigMap, key string) int {
	if cm == nil {
		return -1
	}
	limit, err := strconv.Atoi(cm.Data[key])
	if err != nil || limit < 0 {
		return -1
	}
	return limit
}
