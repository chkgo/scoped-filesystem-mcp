package platform

import (
	"golang.org/x/sys/unix"
	"testing"
)

func TestDarwinRecognizesBothUnsupportedErrnos(t *testing.T) {
	for _, err := range []error{unix.ENOTSUP, unix.EOPNOTSUPP} {
		if !renameUnsupported(err) {
			t.Fatalf("not recognized: %v", err)
		}
	}
	if renameUnsupported(unix.EPERM) {
		t.Fatal("permission failure misclassified as missing capability")
	}
}
