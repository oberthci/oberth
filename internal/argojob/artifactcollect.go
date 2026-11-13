package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	collectPodSuffix          = "-collect"
	artifactCollectionTimeout = 2 * time.Minute
	helperCleanupTimeout      = 5 * time.Second

	DefaultArtifactsLimitBytes = 256 << 20
)

var ErrArtifactsTooLarge = errors.New("argojob: artifacts exceed the configured limit")

func (seeder *SourceSeeder) Collect(ctx context.Context, workflowName string) (archive []byte, collectErr error) {
	ctx, cancel := context.WithTimeout(ctx, artifactCollectionTimeout)
	defer cancel()
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			archive = nil
			collectErr = fmt.Errorf("argojob: artifact collection deadline exceeded after %s: %w", artifactCollectionTimeout, context.DeadlineExceeded)
		}
	}()
	claimName := sourceClaimName(workflowName)
	podName := claimName + collectPodSuffix
	expected, err := seeder.createSeedPodBound(ctx, podName, claimName)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Stop the collection timer before the separate cleanup allowance:
		// a completed archive must not expire while DELETE is slow.
		cancel()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), helperCleanupTimeout)
		defer cancelCleanup()
		options := *metav1.NewDeleteOptions(0)
		if expected != nil && expected.UID != "" {
			options.Preconditions = &metav1.Preconditions{UID: &expected.UID}
		}
		_ = seeder.client.CoreV1().Pods(seeder.config.Namespace).Delete(cleanupCtx, podName, options)
	}()
	if err := seeder.waitForSeedPod(ctx, podName, expected); err != nil {
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

// A crash may leave the canonical collector alive. Replace only a helper owned
// by this exact Workflow UID and mounting this claim; never execute its process.
func (seeder *SourceSeeder) removeLeftoverCollector(ctx context.Context, expected *corev1.Pod, claimName string) error {
	pods := seeder.client.CoreV1().Pods(seeder.config.Namespace)
	previous, err := pods.Get(ctx, expected.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("argojob: read leftover collector: %w", err)
	}
	shape := previous.DeepCopy()
	shape.OwnerReferences = nil
	if previous.UID == "" || previous.ResourceVersion == "" || previous.Namespace != expected.Namespace ||
		len(previous.OwnerReferences) != 1 || previous.OwnerReferences[0] != expected.OwnerReferences[0] ||
		!isSourceSeedForClaim(shape, claimName) {
		return errors.New("argojob: leftover collector ownership or source claim differs")
	}
	options := *metav1.NewDeleteOptions(0)
	options.Preconditions = &metav1.Preconditions{UID: &previous.UID, ResourceVersion: &previous.ResourceVersion}
	if err := pods.Delete(ctx, previous.Name, options); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("argojob: remove leftover collector: %w", err)
	}
	for {
		current, err := pods.Get(ctx, previous.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("argojob: observe leftover collector deletion: %w", err)
		}
		if current.UID != previous.UID {
			return errors.New("argojob: collector changed during deletion")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(seeder.interval()):
		}
	}
}
