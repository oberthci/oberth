package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/resourcesampler"
)

type metricsOnlyResourceSource struct{}

func (metricsOnlyResourceSource) Interval() time.Duration { return time.Second }
func (metricsOnlyResourceSource) Snapshot(context.Context, string) (resourcesampler.Snapshot, error) {
	panic("metrics must not start a scrape")
}
func (metricsOnlyResourceSource) WriteMetrics(w io.Writer) {
	_, _ = fmt.Fprint(w, "oberth_resource_sampler_scrape_failures_total 3\n")
}
func TestArgoResourceMetricsReachServerAdapterWithoutSampling(t *testing.T) {
	controller := &argojob.Controller{}
	jobs := &ArgoJobs{controller: controller}
	var out bytes.Buffer
	jobs.WriteResourceMetrics(&out)
	if out.Len() != 0 {
		t.Fatal("disabled sampler exposed metrics", out.String())
	}
	controller.WithResourceSampler(metricsOnlyResourceSource{}, nil)
	jobs.WriteResourceMetrics(&out)
	if out.String() != "oberth_resource_sampler_scrape_failures_total 3\n" {
		t.Fatal("lost sampler metrics", out.String())
	}
}
