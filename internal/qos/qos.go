// Package qos computes a pod's Kubernetes QoS class as a pure function of
// its spec, reproducing the classification kubelet itself runs server-side
// (k8s.io/kubernetes/pkg/apis/core/v1/helper/qos). See resolution 14 in
// spec_default.md: the pod-cgroup path depends on this class, and
// pod.Status.QOSClass is not reliably available yet on a pod the informer
// has just delivered — spec is the only value available on the hot path,
// so it must be the source of truth, with status used purely as a sanity
// check.
package qos

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/azalio/cpu-idle-operator/internal/cgroup"
)

// Class is a Kubernetes pod QoS class, computed purely from spec.
type Class string

const (
	// Guaranteed pods have matching non-zero CPU and memory requests and
	// limits, either at pod level or independently on every regular/init
	// container.
	Guaranteed Class = "Guaranteed"
	// Burstable pods have at least one request or limit set somewhere, but
	// do not qualify as Guaranteed.
	Burstable Class = "Burstable"
	// BestEffort pods have no CPU or memory requests/limits at pod level or
	// on any regular/init container.
	BestEffort Class = "BestEffort"
)

// ToCgroupClass maps c (this package's spec-derived, corev1-style QoS
// class) onto cgroup.QoSClass (the lowercase vocabulary
// cgroup.PodCgroupPath expects). internal/cgroup redeclares its own
// QoSClass type rather than importing this package (see its doc comment),
// so this is the one place the mapping between the two vocabularies
// happens; every caller that needs a pod's cgroup path from a qos.Class
// must go through it rather than keeping its own copy, so an applier's
// computed path and a reconciler's computed path can never diverge.
func ToCgroupClass(c Class) cgroup.QoSClass {
	switch c {
	case Guaranteed:
		return cgroup.QoSGuaranteed
	case Burstable:
		return cgroup.QoSBurstable
	default:
		return cgroup.QoSBestEffort
	}
}

// supportedResources are the only resource names kubelet's own QoS
// computation considers. Anything else (ephemeral-storage, extended
// resources) is ignored, exactly as it is server-side.
var supportedResources = [...]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// ClassOf computes spec's QoS class the same way the Kubernetes 1.36
// kubelet used by this module does. With PodLevelResources enabled, any
// non-nil spec.resources takes precedence, including an explicitly empty
// object. Kubernetes 1.37 introduced a feature-gated fix that ignores the
// empty shape; the operator must be upgraded in lockstep with that behavior
// before changing this testable compatibility rule. Otherwise each regular
// and init container must independently have matching, positive CPU and
// memory requests and limits for the pod to be Guaranteed.
func ClassOf(spec corev1.PodSpec) Class {
	if spec.Resources != nil {
		return requirementsClass(spec.Resources)
	}

	allContainers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	allContainers = append(allContainers, spec.Containers...)
	allContainers = append(allContainers, spec.InitContainers...)

	var class Class
	for _, container := range allContainers {
		containerClass := requirementsClass(&container.Resources)
		if containerClass == Burstable {
			return Burstable
		}
		if class == "" {
			class = containerClass
		} else if class != containerClass {
			return Burstable
		}
	}

	if class == "" {
		return BestEffort
	}
	return class
}

func requirementsClass(resources *corev1.ResourceRequirements) Class {
	class := Class("")
	for _, name := range supportedResources {
		request := resources.Requests[name]
		limit := resources.Limits[name]

		resourceClass := Guaranteed
		if !request.Equal(limit) {
			resourceClass = Burstable
		} else if request.IsZero() {
			resourceClass = BestEffort
		}

		if resourceClass == Burstable {
			return Burstable
		}
		if class == "" {
			class = resourceClass
		} else if class != resourceClass {
			return Burstable
		}
	}
	return class
}

// VerifyAgainstStatus compares computed — the spec-derived Class, always
// authoritative — against status, the value kubelet wrote to
// pod.Status.QOSClass. computed is a plain value and is never altered by
// this call; the flag and message are for logging only. An empty status is
// not a mismatch: a freshly created pod has not had its status populated
// yet, and that is expected, not an error.
func VerifyAgainstStatus(computed Class, status corev1.PodQOSClass) (mismatch bool, message string) {
	if status == "" {
		return false, ""
	}
	if string(computed) == string(status) {
		return false, ""
	}
	return true, fmt.Sprintf(
		"qos: computed class %q disagrees with pod status.qosClass %q; the computed value remains authoritative",
		computed, status,
	)
}

// HasCPUQuota predicts whether kubelet creates a finite cpu.max for spec's
// pod cgroup when CPU quota enforcement is enabled. The live cgroup remains
// authoritative because enforcement may be disabled or convergence delayed.
// BestEffort never receives a pod quota, even if Kubernetes 1.36's empty
// pod-level resource override hid container limits while classifying it. For
// Burstable/Guaranteed, a positive pod-level CPU limit overrides incomplete
// container limits; without it every regular and init container must have a
// positive CPU limit.
func HasCPUQuota(spec corev1.PodSpec) bool {
	if ClassOf(spec) == BestEffort {
		return false
	}
	pod := &corev1.Pod{Spec: spec}
	if resourcehelper.IsPodLevelResourcesSet(pod) && spec.Resources.Limits.Cpu().Sign() > 0 {
		return true
	}
	for _, container := range spec.Containers {
		if container.Resources.Limits.Cpu().Sign() <= 0 {
			return false
		}
	}
	for _, container := range spec.InitContainers {
		if container.Resources.Limits.Cpu().Sign() <= 0 {
			return false
		}
	}
	return true
}
