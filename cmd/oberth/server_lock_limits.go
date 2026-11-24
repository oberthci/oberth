package main

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
	"strings"
)

func (o *serverOwners) lockLimits(ctx context.Context, locks []service.Lock) error {
	limits := map[string]int{}
	for i := range locks {
		l := &locks[i]
		if limit, ok := limits[l.Name]; ok {
			l.Limit = limit
			continue
		}
		parts := strings.Split(l.Name, "/")
		limit := 1
		if len(parts) == 4 && parts[1] == "ConfigMap" {
			cm, err := o.cluster.Kube.CoreV1().ConfigMaps(parts[0]).Get(ctx, parts[2], meta.GetOptions{})
			if err != nil {
				return err
			}
			limit, err = strconv.Atoi(cm.Data[parts[3]])
			if err != nil || limit < 0 {
				return errors.New("native lock limit is invalid")
			}
		}
		l.Limit, limits[l.Name] = limit, limit
	}
	return nil
}
