package envgate

import (
	"errors"
	"testing"
)

func TestKernelReason(t *testing.T) {
	tests := []struct {
		name  string
		uname UnameFunc
		want  Reason
	}{
		{name: "nil uname", want: ReasonKernelUnknown},
		{name: "uname failure", uname: func() (string, error) {
			return "", errors.New("uname unavailable")
		}, want: ReasonKernelUnknown},
		{name: "unparseable", uname: func() (string, error) {
			return "vendor-kernel", nil
		}, want: ReasonKernelUnknown},
		{name: "too old", uname: func() (string, error) {
			return "5.14.99", nil
		}, want: ReasonKernelTooOld},
		{name: "minimum", uname: func() (string, error) {
			return "5.15.0", nil
		}, want: ReasonOK},
		{name: "new distro kernel", uname: func() (string, error) {
			return "6.17.0-061700-generic", nil
		}, want: ReasonOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := kernelReason(tc.uname); got != tc.want {
				t.Fatalf("kernelReason() = %q, want %q", got, tc.want)
			}
		})
	}
}
