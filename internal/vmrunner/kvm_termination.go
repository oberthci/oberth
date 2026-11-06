package vmrunner

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/protectedtests"
)

type KVMTerminationBinding = protectedtests.TerminationBinding

func VerifyKVMTermination(binding KVMTerminationBinding, pod *corev1.Pod) error {
	return protectedtests.VerifyTermination(binding, pod)
}
