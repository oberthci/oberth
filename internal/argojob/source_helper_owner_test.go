package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	ktesting "k8s.io/client-go/testing"
)

func TestSourceHelpersFollowWorkflowUIDThroughGarbageCollection(t *testing.T) {
	for _, patchFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct-owner", true: "crash-before-pod-patch"}[patchFails], func(t *testing.T) {
			client := collectorTestClient()
			namespace := seedTestConfig().Namespace
			claimName := sourceClaimName("oberth-run-abc")
			claim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), claimName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			claim.OwnerReferences = nil // Workflow does not exist during seeding.
			if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				a.(ktesting.CreateAction).GetObject().(*corev1.Pod).UID = types.UID(a.(ktesting.CreateAction).GetObject().(*corev1.Pod).Name + "-uid")
				return false, nil, nil
			})
			seeder := NewSourceSeeder(client, recordingExec(nil), seedTestConfig())
			seed, err := seeder.createSeedPodBound(t.Context(), claimName, claimName)
			if err != nil {
				t.Fatal(err)
			}
			if len(seed.OwnerReferences) != 0 {
				t.Fatal("seed claimed a Workflow before it existed", seed.OwnerReferences)
			}
			if patchFails {
				client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("server stopped before Pod patch")
				})
			}
			err = seeder.AdoptClaim(t.Context(), claimName, "oberth-run-abc", "workflow-uid")
			if (err != nil) != patchFails {
				t.Fatal(err)
			}
			seed, err = client.CoreV1().Pods(namespace).Get(t.Context(), claimName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !patchFails {
				assertWorkflowHelperOwner(t, seed)
				if err := seeder.AdoptClaim(t.Context(), claimName, "oberth-run-abc", "workflow-uid"); err != nil {
					t.Fatal("adoption is not idempotent", err)
				}
			}
			collector, err := seeder.createSeedPodBound(t.Context(), claimName+collectPodSuffix, claimName)
			if err != nil {
				t.Fatal(err)
			}
			assertWorkflowHelperOwner(t, collector)
			if collector.Spec.ActiveDeadlineSeconds == nil || *collector.Spec.ActiveDeadlineSeconds != 120 {
				t.Fatal("collector has no Kubernetes deadline")
			}
			modelSourceHelperGC(t, client, "workflow-uid")
			pods, _ := client.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{})
			if len(pods.Items) != 2 {
				t.Fatal("live Workflow helpers collected")
			}
			// Same Workflow name with a new UID must not retain either old helper.
			modelSourceHelperGC(t, client, "replacement-workflow-uid")
			pods, _ = client.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{})
			if patchFails {
				if len(pods.Items) != 1 {
					t.Fatal("expected only the pre-adoption seed crash gap", pods.Items)
				}
				seeder.SweepOrphanedSourceClaims(t.Context())
				modelSourceHelperGC(t, client, "replacement-workflow-uid")
				pods, _ = client.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{})
			}
			if len(pods.Items) != 0 {
				t.Fatal("helpers leaked after Workflow deletion/sweep", pods.Items)
			}
		})
	}
}

func assertWorkflowHelperOwner(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if len(pod.OwnerReferences) != 1 {
		t.Fatal(pod.OwnerReferences)
	}
	owner := pod.OwnerReferences[0]
	if owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != "oberth-run-abc" || owner.UID != "workflow-uid" || owner.Controller != nil || owner.BlockOwnerDeletion != nil {
		t.Fatal(owner)
	}
}

// The fake API has no GC controller. Model only Kubernetes' same-namespace,
// UID-bound background owner deletion, including PVC protection while a
// helper still references the claim. Ownerless crash leftovers need the sweep.
func modelSourceHelperGC(t *testing.T, client *fake.Clientset, liveWorkflowUID types.UID) {
	t.Helper()
	ns := seedTestConfig().Namespace
	claims, err := client.CoreV1().PersistentVolumeClaims(ns).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims.Items {
		for _, owner := range claim.OwnerReferences {
			if owner.Kind == "Workflow" && owner.UID != liveWorkflowUID {
				pods, _ := client.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{})
				inUse := false
				for _, pod := range pods.Items {
					for _, v := range pod.Spec.Volumes {
						if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim.Name {
							inUse = true
						}
					}
				}
				if inUse {
					now := metav1.Now()
					claim.DeletionTimestamp = &now
					if _, err := client.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), &claim, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				} else if err := client.CoreV1().PersistentVolumeClaims(ns).Delete(t.Context(), claim.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	pods, err := client.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if len(pod.OwnerReferences) == 0 {
			continue
		}
		if len(pod.OwnerReferences) != 1 {
			t.Fatal(pod.OwnerReferences)
		}
		owner := pod.OwnerReferences[0]
		dead := owner.Kind == "Workflow" && owner.UID != liveWorkflowUID
		if owner.Kind == "PersistentVolumeClaim" {
			claim, err := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), owner.Name, metav1.GetOptions{})
			dead = apierrors.IsNotFound(err) || (err == nil && claim.UID != owner.UID)
		}
		if dead {
			if err := client.CoreV1().Pods(ns).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSeedCrashBeforeWorkflowCreationIsSweptThroughClaim(t *testing.T) {
	client := collectorTestClient()
	ns := seedTestConfig().Namespace
	name := sourceClaimName("oberth-run-abc")
	claim, _ := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), name, metav1.GetOptions{})
	claim.OwnerReferences = nil
	claim.Labels = map[string]string{"oberth.ci/component": "source", "oberth.ci/workflow": "oberth-run-abc"}
	claim.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	seeder := NewSourceSeeder(client, recordingExec(nil), seedTestConfig())
	if _, err := seeder.createSeedPodBound(t.Context(), name, name); err != nil {
		t.Fatal(err)
	}
	seeder.SweepOrphanedSourceClaims(t.Context())
	modelSourceHelperGC(t, client, "")
	pods, _ := client.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatal("seed leaked after orphan claim sweep")
	}
}

func TestCollectDeadlineBoundsPendingExecAndCleanup(t *testing.T) {
	for _, phase := range []string{"pending", "exec", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := collectorTestClient()
				if phase != "pending" {
					runningSeedPods(client)
				}
				execCalls := 0
				seeder := NewSourceSeeder(client, func(ctx context.Context, _, _, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
					execCalls++
					<-ctx.Done()
					return ctx.Err()
				}, seedTestConfig())
				if phase == "cleanup" {
					seeder.client = stalledDeleteKube{Interface: client}
				}
				start := time.Now()
				_, err := seeder.Collect(context.Background(), "oberth-run-abc")
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "artifact collection deadline exceeded") {
					t.Fatal(err)
				}
				want := artifactCollectionTimeout
				if phase == "cleanup" {
					want += helperCleanupTimeout
				}
				if time.Since(start) != want {
					t.Fatal("unbounded collection", time.Since(start))
				}
				if (execCalls == 0) != (phase == "pending") {
					t.Fatal("unexpected exec", execCalls)
				}
				if phase != "cleanup" {
					pods, _ := client.CoreV1().Pods(seedTestConfig().Namespace).List(t.Context(), metav1.ListOptions{})
					if len(pods.Items) != 0 {
						t.Fatal("collector leaked")
					}
				}
			})
		})
	}
}

func TestCollectRequiresWorkflowOwnerBeforeCreatingPod(t *testing.T) {
	for _, uid := range []types.UID{"", "wrong-owner"} {
		client := collectorTestClient()
		ns := seedTestConfig().Namespace
		claim, _ := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), sourceClaimName("oberth-run-abc"), metav1.GetOptions{})
		claim.OwnerReferences[0].UID = uid
		if uid != "" {
			claim.OwnerReferences[0].Name = "foreign-workflow"
		}
		if _, err := client.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		seeder := NewSourceSeeder(client, recordingExec(nil), seedTestConfig())
		if _, err := seeder.Collect(t.Context(), "oberth-run-abc"); err == nil {
			t.Fatal("accepted missing/foreign Workflow owner")
		}
		if len(createdPods(t, client)) != 0 {
			t.Fatal("created an unowned collector")
		}
	}
}

// Typed wrapper retains the request context which client-go's fake discards.
type stalledDeleteKube struct{ kubernetes.Interface }

func (c stalledDeleteKube) CoreV1() typedcorev1.CoreV1Interface {
	return stalledDeleteCore{CoreV1Interface: c.Interface.CoreV1()}
}

type stalledDeleteCore struct{ typedcorev1.CoreV1Interface }

func (c stalledDeleteCore) Pods(ns string) typedcorev1.PodInterface {
	return stalledDeletePods{PodInterface: c.CoreV1Interface.Pods(ns)}
}

type stalledDeletePods struct{ typedcorev1.PodInterface }

func (p stalledDeletePods) Delete(ctx context.Context, _ string, _ metav1.DeleteOptions) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSeedSweepPreservesFreshReplacementAndOwnedPods(t *testing.T) {
	for _, mode := range []string{"fresh", "fresh-on-unowned-claim", "foreign-shape", "workflow-owned"} {
		t.Run(mode, func(t *testing.T) {
			client := collectorTestClient()
			ns := seedTestConfig().Namespace
			name := sourceClaimName("oberth-run-abc")
			seeder := NewSourceSeeder(client, recordingExec(nil), seedTestConfig())
			pod, err := seeder.createSeedPodBound(t.Context(), name, name)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "fresh", "fresh-on-unowned-claim":
				pod.CreationTimestamp = metav1.Now()
				if mode == "fresh-on-unowned-claim" {
					claim, _ := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), name, metav1.GetOptions{})
					claim.OwnerReferences = nil
					if _, err := client.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-shape":
				pod.Spec.Containers[0].Name = "foreign"
			case "workflow-owned":
				pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "oberth-run-abc", UID: "workflow-uid"}}
			}
			if _, err := client.CoreV1().Pods(ns).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			seeder.SweepOrphanedSourceClaims(t.Context())
			if _, err := client.CoreV1().Pods(ns).Get(t.Context(), name, metav1.GetOptions{}); err != nil {
				t.Fatal("sweep removed another attempt or owned Pod", err)
			}
			if _, err := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), name, metav1.GetOptions{}); err != nil {
				t.Fatal("sweep removed claim under preserved seed", err)
			}
		})
	}
}

func TestCollectKeepsCompletedArchiveDuringSlowCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := collectorTestClient()
		runningSeedPods(client)
		seeder := NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
			time.Sleep(artifactCollectionTimeout - time.Second)
			_, err := stdout.Write([]byte("complete archive"))
			return err
		}, seedTestConfig())
		seeder.client = stalledDeleteKube{Interface: client}
		start := time.Now()
		archive, err := seeder.Collect(context.Background(), "oberth-run-abc")
		if err != nil || string(archive) != "complete archive" {
			t.Fatal("cleanup expired an already completed collection", string(archive), err)
		}
		if time.Since(start) != artifactCollectionTimeout-time.Second+helperCleanupTimeout {
			t.Fatal("cleanup was not bounded", time.Since(start))
		}
	})
}

func TestCollectReplacesOnlyMatchingWorkflowCollector(t *testing.T) {
	for _, variant := range []string{"owned", "foreign-owner", "foreign-claim", "ownerless", "missing-uid", "missing-version", "delete-race"} {
		t.Run(variant, func(t *testing.T) {
			client := collectorTestClient()
			runningSeedPods(client)
			ns := seedTestConfig().Namespace
			claimName := sourceClaimName("oberth-run-abc")
			execs := 0
			seeder := NewSourceSeeder(client, func(ctx context.Context, _, name, _ string, _ []string, _ io.Reader, out, _ io.Writer) error {
				execs++
				pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil || pod.UID == "crashed-collector" {
					t.Fatal("executed leftover", pod, err)
				}
				_, err = out.Write([]byte("collected again"))
				return err
			}, seedTestConfig())
			old, err := seeder.createSeedPodBound(t.Context(), claimName+collectPodSuffix, claimName)
			if err != nil {
				t.Fatal(err)
			}
			old.UID = "crashed-collector"
			switch variant {
			case "foreign-owner":
				old.OwnerReferences[0].UID = "another-workflow"
			case "foreign-claim":
				old.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "another-claim"
			case "ownerless":
				old.OwnerReferences = nil
			case "missing-uid":
				old.UID = ""
			case "missing-version":
				old.ResourceVersion = ""
			}
			if _, err = client.CoreV1().Pods(ns).Update(t.Context(), old, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			start := len(client.Actions())
			client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil {
					t.Fatal("unbound collector delete")
				}
				if *options.Preconditions.UID == old.UID {
					if options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != old.ResourceVersion {
						t.Fatal("collector owner change not fenced")
					}
					if variant == "delete-race" {
						return true, nil, apierrors.NewConflict(corev1.Resource("pods"), old.Name, errors.New("replaced before delete"))
					}
				}
				return false, nil, nil
			})
			archive, err := seeder.Collect(t.Context(), "oberth-run-abc")
			creates, deletes := 0, 0
			for _, action := range client.Actions()[start:] {
				if action.GetResource().Resource != "pods" {
					continue
				}
				if action.GetVerb() == "create" {
					creates++
				}
				if action.GetVerb() == "delete" {
					deletes++
				}
			}
			if variant == "owned" {
				if err != nil || string(archive) != "collected again" || creates != 2 || deletes != 2 || execs != 1 {
					t.Fatal(string(archive), err, creates, deletes, execs)
				}
			} else {
				wantDeletes := 0
				if variant == "delete-race" {
					wantDeletes = 1
				}
				if err == nil || len(archive) != 0 || execs != 0 || creates != 1 || deletes != wantDeletes {
					t.Fatal(string(archive), err, creates, deletes, execs)
				}
				if _, err := client.CoreV1().Pods(ns).Get(t.Context(), old.Name, metav1.GetOptions{}); err != nil {
					t.Fatal("foreign/raced collector deleted", err)
				}
			}
		})
	}
}

func TestCollectLeftoverDeletionUsesCollectionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := collectorTestClient()
		seeder := NewSourceSeeder(client, recordingExec(nil), seedTestConfig())
		claim := sourceClaimName("oberth-run-abc")
		if _, err := seeder.createSeedPodBound(t.Context(), claim+collectPodSuffix, claim); err != nil {
			t.Fatal(err)
		}
		// Accepted DELETE can remain pending; never create over that old name.
		client.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
		start := time.Now()
		if archive, err := seeder.Collect(t.Context(), "oberth-run-abc"); !errors.Is(err, context.DeadlineExceeded) || len(archive) != 0 || time.Since(start) != artifactCollectionTimeout {
			t.Fatal(err, len(archive), time.Since(start))
		}
	})
}

func TestSourceHelperOwnerRetriesTransientClaimAdoption(t *testing.T) {
	client := collectorTestClient()
	ns := seedTestConfig().Namespace
	claimName := sourceClaimName("oberth-run-abc")
	claim, err := client.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), claimName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim.OwnerReferences = nil
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	updates := 0
	client.PrependReactor("update", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, errors.New("transient ownership write failure")
		}
		return false, nil, nil
	})
	runningSeedPods(client)
	seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}, seedTestConfig())
	controller := &Controller{seeder: seeder}
	controller.adoptClaimBestEffort(t.Context(), claimName, "oberth-run-abc", "workflow-uid")
	if updates != 2 {
		t.Fatal("adoption was not retried once", updates)
	}
	if _, err := seeder.Collect(t.Context(), "oberth-run-abc"); err != nil {
		t.Fatal("retry did not permit collection", err)
	}
}
