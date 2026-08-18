// Package qos computes a pod's Kubernetes QoS class as a pure function of
// its spec, reproducing the accumulation kubelet itself runs server-side
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

	"github.com/azalio/cpi-idle-operator/internal/cgroup"
)

// Class is a Kubernetes pod QoS class, computed purely from spec.
type Class string

const (
	// Guaranteed pods have every container's limits set on both CPU and
	// memory, with the pod-wide sum of requests equal to the pod-wide sum
	// of limits for every resource that appears.
	Guaranteed Class = "Guaranteed"
	// Burstable pods have at least one request or limit set somewhere, but
	// do not qualify as Guaranteed.
	Burstable Class = "Burstable"
	// BestEffort pods have no requests and no limits set anywhere, on any
	// container.
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

// ClassOf computes spec's QoS class the same way kubelet does: it reads
// spec.Containers and spec.InitContainers only, and never anything under
// status. A container counts toward Guaranteed only when it has a strictly
// positive limit set for every supported resource; the pod is Guaranteed
// only when every container clears that bar and the pod-wide sums of
// requests and limits agree for every resource that appears.
func ClassOf(spec corev1.PodSpec) Class {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true

	allContainers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	allContainers = append(allContainers, spec.Containers...)
	allContainers = append(allContainers, spec.InitContainers...)

	for _, container := range allContainers {
		addPositive(requests, container.Resources.Requests)
		addPositive(limits, container.Resources.Limits)

		limitsFound := 0
		for _, name := range supportedResources {
			if hasPositive(container.Resources.Limits, name) {
				limitsFound++
			}
		}
		if limitsFound != len(supportedResources) {
			// Intent: a single container missing a limit on any supported
			// resource disqualifies the whole pod from Guaranteed, even if
			// every other container is fully specified.
			isGuaranteed = false
		}
	}

	if len(requests) == 0 && len(limits) == 0 {
		return BestEffort
	}

	if isGuaranteed {
		for name, req := range requests {
			lim, ok := limits[name]
			if !ok || lim.Cmp(req) != 0 {
				isGuaranteed = false
				break
			}
		}
	}

	if isGuaranteed && len(requests) == len(limits) {
		return Guaranteed
	}
	return Burstable
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

// addPositive accumulates the supported resources with a strictly positive
// quantity from src into dst, matching kubelet: a request or limit of
// exactly zero (or absent) does not count toward QoS.
func addPositive(dst corev1.ResourceList, src corev1.ResourceList) {
	for _, name := range supportedResources {
		quantity, ok := src[name]
		if !ok || quantity.Sign() <= 0 {
			continue
		}
		if existing, ok := dst[name]; ok {
			existing.Add(quantity)
			dst[name] = existing
		} else {
			dst[name] = quantity.DeepCopy()
		}
	}
}

// hasPositive reports whether list carries a strictly positive quantity for
// name.
func hasPositive(list corev1.ResourceList, name corev1.ResourceName) bool {
	quantity, ok := list[name]
	return ok && quantity.Sign() > 0
}
