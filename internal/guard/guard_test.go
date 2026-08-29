package guard

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The decider must need two consecutive far-side samples per transition,
// in both directions, so one noisy sample never flips the node.
func TestDeciderHysteresis(t *testing.T) {
	d := &decider{}
	high, low := 0.70, 0.60

	steps := []struct {
		util float64
		hot  bool
	}{
		{0.50, false}, // cool, below everything
		{0.75, false}, // first hot sample: not yet
		{0.55, false}, // streak broken
		{0.75, false}, // first again
		{0.80, true},  // second consecutive: hot
		{0.65, true},  // inside the band: stays hot
		{0.55, true},  // first cool sample: not yet
		{0.75, true},  // streak broken, still hot
		{0.55, true},  // first again
		{0.50, false}, // second consecutive: cool
	}
	for i, step := range steps {
		if got := d.observe(step.util, high, low); got != step.hot {
			t.Fatalf("step %d: util=%v got hot=%v, want %v", i, step.util, got, step.hot)
		}
	}
}

func TestHasPositiveCPULimit(t *testing.T) {
	limited := corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		}},
	}}}
	if !hasPositiveCPULimit(limited) {
		t.Fatal("500m limit not detected")
	}
	unlimited := corev1.PodSpec{Containers: []corev1.Container{{}}}
	if hasPositiveCPULimit(unlimited) {
		t.Fatal("no-limit pod reported as limited")
	}
	initLimited := corev1.PodSpec{
		Containers: []corev1.Container{{}},
		InitContainers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			}},
		}},
	}
	if !hasPositiveCPULimit(initLimited) {
		t.Fatal("init container limit not detected")
	}
}

func TestReadUsageUsec(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cpu.stat")
	if err := os.WriteFile(p, []byte("usage_usec 123456\nuser_usec 100\nsystem_usec 200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUsageUsec(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != 123456 {
		t.Fatalf("got %d, want 123456", got)
	}
	if err := os.WriteFile(p, []byte("nr_periods 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readUsageUsec(p); err == nil {
		t.Fatal("expected error for cpu.stat without usage_usec")
	}
}
