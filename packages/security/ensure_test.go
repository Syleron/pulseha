package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// certFixture generates a real set of certificates into a temp dir and returns
// it, along with the bytes of each file as first written.
func certFixture(t *testing.T, hostname string) (dir string, before map[string][]byte) {
	t.Helper()

	dir = t.TempDir()
	prev := CertDir
	CertDir = dir
	t.Cleanup(func() { CertDir = prev })

	if err := GenerateCertificates(hostname); err != nil {
		t.Fatalf("GenerateCertificates: %v", err)
	}

	before = map[string][]byte{}
	for _, name := range []string{"ca.crt", "ca.key", "pulseha.crt", "pulseha.key"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		before[name] = b
	}
	return dir, before
}

func filesUnchanged(t *testing.T, dir string, before map[string][]byte) bool {
	t.Helper()

	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			return false
		}
	}
	return true
}

// Regression for docs/TEST-PLAN.md #111's prerequisite, and ADR-0005's first
// consequence: a certificate that changes on restart cannot be in an allowlist.
//
// GenerateCertificates rewrote all four files unconditionally, so a node's
// identity changed every time the daemon came up — observed on a lab appliance
// with the CA written one second after the process started.
func TestUsableCertificatesAreKept(t *testing.T) {
	dir, before := certFixture(t, "node-a")

	for i := range 3 {
		generated, reason, err := EnsureCertificates("node-a")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if generated {
			t.Fatalf("run %d regenerated usable certificates (%s)", i, reason)
		}
	}
	if !filesUnchanged(t, dir, before) {
		t.Error("certificate files changed while being kept; the identity is not stable")
	}
}

// The positive control. Without it, a check that never regenerates anything would
// satisfy the test above by doing nothing at all.
func TestCertificatesAreGeneratedWhenMissing(t *testing.T) {
	dir := t.TempDir()
	prev := CertDir
	CertDir = dir
	t.Cleanup(func() { CertDir = prev })

	generated, reason, err := EnsureCertificates("node-a")
	if err != nil {
		t.Fatalf("EnsureCertificates: %v", err)
	}
	if !generated {
		t.Fatal("nothing generated into an empty directory")
	}
	if !strings.Contains(reason, "missing") {
		t.Errorf("reason = %q, want it to say what was missing", reason)
	}
	for _, name := range []string{"ca.crt", "ca.key", "pulseha.crt", "pulseha.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// Each way the material on disk can be unusable, and each one has to be caught:
// what is there is only this node's own identity, so replacing what cannot be
// read costs nothing that was working.
func TestUnusableCertificatesAreReplaced(t *testing.T) {
	cases := map[string]struct {
		break_ func(t *testing.T, dir string)
		want   string
	}{
		"a missing file": {
			break_: func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "ca.crt")) },
			want:   "missing",
		},
		"a truncated file": {
			break_: func(t *testing.T, dir string) {
				os.WriteFile(filepath.Join(dir, "pulseha.crt"), []byte("not pem"), 0644)
			},
			want: "not a PEM certificate",
		},
		"a node certificate signed by a different CA": {
			// The half-written state: the two files are written separately, so a
			// crash between the renames leaves a pair that does not belong together
			// and that nobody else can verify.
			break_: func(t *testing.T, dir string) {
				other := t.TempDir()
				prev := CertDir
				CertDir = other
				if err := GenerateCertificates("node-a"); err != nil {
					t.Fatalf("second CA: %v", err)
				}
				CertDir = prev
				b, _ := os.ReadFile(filepath.Join(other, "ca.crt"))
				os.WriteFile(filepath.Join(dir, "ca.crt"), b, 0644)
			},
			want: "not signed by the CA",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := certFixture(t, "node-a")
			tc.break_(t, dir)

			generated, reason, err := EnsureCertificates("node-a")
			if err != nil {
				t.Fatalf("EnsureCertificates: %v", err)
			}
			if !generated {
				t.Fatalf("kept unusable certificates (reason %q)", reason)
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}

// A renamed host needs a new certificate; the alternative is a node presenting a
// name it no longer has.
func TestARenamedHostGetsANewCertificate(t *testing.T) {
	certFixture(t, "node-a")

	generated, reason, err := EnsureCertificates("node-b")
	if err != nil {
		t.Fatalf("EnsureCertificates: %v", err)
	}
	if !generated {
		t.Fatal("kept a certificate naming the old hostname")
	}
	if !strings.Contains(reason, "node-a") || !strings.Contains(reason, "node-b") {
		t.Errorf("reason = %q, want it to name both the old and the new host", reason)
	}
}

// Expiry is checked against a clock rather than time.Now, so the renewal window
// can be exercised without waiting a year for it.
func TestExpiryAndRenewalAreDetected(t *testing.T) {
	dir, _ := certFixture(t, "node-a")

	t.Run("valid today", func(t *testing.T) {
		if reason := certsUnusable(dir, "node-a", time.Now()); reason != "" {
			t.Errorf("fresh certificates read as unusable: %s", reason)
		}
	})

	t.Run("inside the renewal window", func(t *testing.T) {
		// The node certificate is valid for a year; a day before that is well
		// inside the 30-day window.
		soon := time.Now().AddDate(1, 0, -1)
		reason := certsUnusable(dir, "node-a", soon)
		if !strings.Contains(reason, "renewal window") {
			t.Errorf("reason = %q, want the renewal window", reason)
		}
	})

	t.Run("after expiry", func(t *testing.T) {
		later := time.Now().AddDate(1, 0, 1)
		reason := certsUnusable(dir, "node-a", later)
		if !strings.Contains(reason, "expired") {
			t.Errorf("reason = %q, want an expiry", reason)
		}
	})
}
