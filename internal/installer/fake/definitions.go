package fake

import (
	"context"

	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Definitions supplies immutable read snapshots without field-management machinery.
type Definitions struct {
	Objects map[string]*definitions.CustomResourceDefinition
	Reads   []string
	Err     error
}

func (d *Definitions) Get(ctx context.Context, name string, _ metav1.GetOptions) (*definitions.CustomResourceDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.Reads = append(d.Reads, name)
	if d.Err != nil {
		return nil, d.Err
	}
	if crd := d.Objects[name]; crd != nil {
		return crd.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, name)
}
