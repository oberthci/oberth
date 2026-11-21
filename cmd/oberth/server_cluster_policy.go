package main

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type clusterPolicy struct{ owner *serverOwners }

func (p clusterPolicy) ValidateCapability(ctx context.Context, c apicontract.Capability) error {
	return p.owner.authority.ValidateCapability(ctx, c)
}
func (p clusterPolicy) ServerNamespace(ns string) bool { return ns == p.owner.c.config.Namespace }
func (p clusterPolicy) Namespaced(ctx context.Context, version, kind string) (bool, error) {
	resource, err := p.resource(ctx, version, kind)
	return resource.Namespaced, err
}
func (p clusterPolicy) resource(ctx context.Context, version, kind string) (meta.APIResource, error) {
	path := "/apis/" + version
	if !strings.Contains(version, "/") {
		path = "/api/" + version
	}
	var resources meta.APIResourceList
	err := p.owner.cluster.Kube.Discovery().RESTClient().Get().AbsPath(path).Do(ctx).Into(&resources)
	if err != nil {
		return meta.APIResource{}, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == kind && !strings.Contains(r.Name, "/") {
			return r, nil
		}
	}
	return meta.APIResource{}, nil
}
