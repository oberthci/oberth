// Package argowatch observes Oberth Workflows and step Pods through two shared,
// namespace-scoped informers. It never writes to Kubernetes or decides run state.
//
// The frozen construction and lifecycle API is:
//
//	New(kube kubernetes.Interface, argo versioned.Interface, namespace string, logger *slog.Logger) (*Hub, error)
//	Run(ctx context.Context)
//
// New requires a nonempty pipeline namespace. A nil logger uses slog.Default.
// Call Run once in a goroutine with the server lifetime context; it blocks until
// shutdown and cannot restart. Call WaitForSync before recovery or dispatch.
//
// The frozen consumer API is:
//
//	Workflow(name string) (*wfv1.Workflow, bool)
//	PodsOf(workflowName string) []*corev1.Pod
//	Pod(name string) (*corev1.Pod, bool)
//	Subscribe(workflowName string) (wake <-chan struct{}, cancel func())
//	Healthy() bool
//	UnhealthySince() (time.Time, bool)
//	WaitForSync(ctx context.Context) error
//
// Cached pointers are shared and read-only: DeepCopy before changing one.
// Subscribe before reading; each event wakes all subscribers for its Workflow,
// and a one-slot channel coalesces bursts. Cancel is idempotent, removes the
// subscription, and does not close the channel. Notifications carry no payload:
// always reread the cache. Confirm cache misses with a live GET before drawing
// destructive conclusions. Reads that gate writes stay live.
//
// Workflow reads and notifications require the Oberth engine label and run-ID
// annotation. Pods require the trigger label and an Argo Workflow controller
// owner matching their workflow label. Consumers must still verify their run ID
// and the Pod owner's UID against the Workflow they are observing; labels alone
// never establish run identity. Pods can arrive before their Workflow.
//
// Healthy is false before both caches sync, after shutdown, or if a resource's
// latest list/watch attempt failed and its last success is over five seconds
// old. UnhealthySince returns the beginning of the current failure episode
// (startup, first failed attempt, or shutdown), with true only while unhealthy.
// A successful attempt clears that resource's failure episode. Overlapping
// outages retain their original timestamp until both resources recover; an idle,
// open watch remains healthy. An API outage is observation unavailability, never a
// run failure. WaitForSync's context should be the server lifetime context.
package argowatch
