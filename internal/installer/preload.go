package installer

// `oberth preload` -- put an image inside the kind node, under whatever name
// the pipeline asks for.
//
// Two problems meet here on an Apple Silicon machine.
//
// The kubelet's presence check is platform-filtered, so an amd64-only image
// sitting in an arm64 node's image store is not "present": a pod with
// imagePullPolicy Never lands in ErrImageNeverPull, and one that is allowed to
// pull lands in a pull of an image that will not run. The image is visibly
// there in `ctr images ls` the whole time, which is what makes this cost an
// afternoon rather than a minute.
//
// The fix is a native-arch image published under a different name, and a
// pipeline that cannot be edited to use that name -- because the name is
// written into someone else's test suite. So: pull the arm64 publication
// inside the node, then tag it as the name the suite expects.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Preload pulls image into every node of the kind cluster and, when as is
// non-empty, tags it there under that second name.
//
// The pull happens INSIDE the node rather than on the host: `kind load` copies
// from the local Docker daemon, which on this machine holds the same
// wrong-architecture image the registry does, so it would faithfully copy the
// problem. containerd in the node resolves the manifest list for its own
// platform.
func Preload(ctx context.Context, deps InstallDeps, cluster, image, as string) error {
	deps = deps.withDefaults()
	image = strings.TrimSpace(image)
	as = strings.TrimSpace(as)
	if image == "" {
		return errors.New("no image to preload")
	}
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		cluster = KindClusterName
	}

	nodes, err := kindNodeNames(ctx, deps.RunCommand, cluster)
	if err != nil {
		return fmt.Errorf("list nodes of kind cluster %q: %w", cluster, err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("kind cluster %q has no nodes", cluster)
	}

	for _, node := range nodes {
		if output, pullErr := deps.RunCommand(ctx, nil, "docker", "exec", node,
			"ctr", "--namespace=k8s.io", "images", "pull", image); pullErr != nil {
			return fmt.Errorf("pull %s inside node %s: %w%s", image, node, pullErr, commandOutputSuffix(output))
		}
		_, _ = fmt.Fprintf(deps.Output, "Pulled %s inside node %s.\n", image, node)

		if as == "" {
			continue
		}
		// --force, because a re-run must be able to move the name onto a newer
		// pull rather than fail on the name it created last time.
		if output, tagErr := deps.RunCommand(ctx, nil, "docker", "exec", node,
			"ctr", "--namespace=k8s.io", "images", "tag", "--force", image, as); tagErr != nil {
			return fmt.Errorf("tag %s as %s inside node %s: %w%s", image, as, node, tagErr, commandOutputSuffix(output))
		}
		_, _ = fmt.Fprintf(deps.Output, "Tagged it as %s inside node %s.\n", as, node)
	}
	return nil
}

// kindNodeNames lists the nodes of a kind cluster by name, which is what
// `docker exec` needs: the node is a container and its name is the handle.
func kindNodeNames(ctx context.Context, run CommandRunner, cluster string) ([]string, error) {
	output, err := run(ctx, nil, "kind", "get", "nodes", "--name", cluster)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(output)), nil
}
