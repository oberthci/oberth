package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	collectPodSuffix = "-collect"

	DefaultArtifactsLimitBytes = 256 << 20
)

var ErrArtifactsTooLarge = errors.New("argojob: artifacts exceed the configured limit")

func (seeder *SourceSeeder) Collect(ctx context.Context, workflowName string) ([]byte, error) {
	claimName := sourceClaimName(workflowName)
	podName := claimName + collectPodSuffix
	if err := seeder.createSeedPod(ctx, podName, claimName); err != nil {
		return nil, err
	}
	defer func() {
		_ = seeder.client.CoreV1().Pods(seeder.config.Namespace).
			Delete(context.WithoutCancel(ctx), podName, *metav1.NewDeleteOptions(0))
	}()
	if err := seeder.waitForSeedPod(ctx, podName); err != nil {
		return nil, err
	}

	limit := seeder.config.ArtifactsLimitBytes
	if limit <= 0 {
		limit = DefaultArtifactsLimitBytes
	}
	source := path.Join(seedMountPath, artifactsSubPath)
	command := []string{"/bin/sh", "-c",
		"if [ -d " + source + " ] && [ -n \"$(ls -A " + source + " 2>/dev/null)\" ]; then " +
			"tar -czf - -C " + source + " .; fi"}

	sink := &boundedSink{limit: limit}
	var stderr strings.Builder
	if err := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, command,
		strings.NewReader(""), sink, &stderr); err != nil {
		if errors.Is(sink.err, ErrArtifactsTooLarge) {
			return nil, fmt.Errorf("%w: over %d bytes", ErrArtifactsTooLarge, limit)
		}
		return nil, fmt.Errorf("argojob: collect artifacts from %s: %w: %s",
			claimName, err, strings.TrimSpace(stderr.String()))
	}
	if sink.err != nil {
		return nil, fmt.Errorf("%w: over %d bytes", ErrArtifactsTooLarge, limit)
	}
	return sink.data, nil
}

type boundedSink struct {
	data  []byte
	limit int64
	err   error
}

func (sink *boundedSink) Write(value []byte) (int, error) {
	if sink.err != nil {
		return 0, sink.err
	}
	if int64(len(sink.data))+int64(len(value)) > sink.limit {
		sink.err = ErrArtifactsTooLarge
		return 0, sink.err
	}
	sink.data = append(sink.data, value...)
	return len(value), nil
}

var _ io.Writer = (*boundedSink)(nil)
