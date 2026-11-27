package network

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The probe has to be the RFC 5227 one. An ordinary request carries this node's
// address as its sender, which is no use on an interface that holds none in the
// subnet and rewrites the neighbours' ARP entry for whatever address it does
// carry.
func TestDuplicateProbeCommandIsADADProbe(t *testing.T) {
	for _, ip := range []string{"172.16.73.43", "172.16.73.43/24", "::ffff:172.16.73.43"} {
		name, args, probe, err := duplicateProbeCommand("ens35", ip)
		if err != nil || !probe {
			t.Fatalf("%s: probe=%v err=%v, want a probe", ip, probe, err)
		}
		if name != "arping" {
			t.Fatalf("%s: binary %q, want arping", ip, name)
		}
		want := []string{"-D", "-f", "-c", "2", "-w", "1", "-I", "ens35", "172.16.73.43"}
		if !slices.Equal(args, want) {
			t.Fatalf("%s: argv %v, want %v", ip, args, want)
		}
	}
}

// The kernel runs duplicate address detection on an IPv6 address itself, and
// arping cannot ask about one.
func TestDuplicateProbeCommandSkipsIPv6(t *testing.T) {
	for _, ip := range []string{"fd00::43", "fd00::43/64"} {
		_, _, probe, err := duplicateProbeCommand("ens35", ip)
		if err != nil || probe {
			t.Fatalf("%s: probe=%v err=%v, want no probe and no error", ip, probe, err)
		}
	}
}

func TestDuplicateProbeCommandRejectsAnUnparseableAddress(t *testing.T) {
	if _, _, probe, err := duplicateProbeCommand("ens35", "not-an-address"); err == nil || probe {
		t.Fatalf("probe=%v err=%v, want an error and no probe", probe, err)
	}
}

func TestInterpretDuplicateProbe(t *testing.T) {
	exited := errors.New("exit status")

	tests := []struct {
		name       string
		output     string
		exitCode   int
		runErr     error
		wantHolder string
		wantHeld   bool
		wantErr    bool
	}{
		{
			name:   "nothing answered",
			output: "ARPING 172.16.73.43 from 0.0.0.0 ens35\nSent 2 probes (2 broadcast(s))\nReceived 0 response(s)\n",
		},
		{
			name: "another host answered",
			output: "ARPING 172.16.73.43 from 0.0.0.0 ens35\n" +
				"Unicast reply from 172.16.73.43 [00:50:56:8C:E5:3A]  0.612ms\n" +
				"Sent 1 probes (1 broadcast(s))\nReceived 1 response(s)\n",
			exitCode:   1,
			runErr:     exited,
			wantHolder: "00:50:56:8c:e5:3a",
			wantHeld:   true,
		},
		{
			// Exit 1 is also what a run that never sent a probe can return.
			// Without a reply in the output there is no evidence of a holder.
			name:     "exit 1 with no reply is a failed probe, not a holder",
			output:   "arping: Device ens35 not available.\n",
			exitCode: 1,
			runErr:   exited,
			wantErr:  true,
		},
		{
			name:     "interface down",
			output:   "arping: Interface \"ens35\" is down\n",
			exitCode: 2,
			runErr:   exited,
			wantErr:  true,
		},
		{
			name:    "binary could not be run",
			runErr:  errors.New("fork/exec arping: permission denied"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			holder, held, err := interpretDuplicateProbe(tt.output, tt.exitCode, tt.runErr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if held != tt.wantHeld || holder != tt.wantHolder {
				t.Fatalf("holder=%q held=%v, want holder=%q held=%v", holder, held, tt.wantHolder, tt.wantHeld)
			}
			// A failed probe must never read as "in use": the caller refuses the
			// address on held alone.
			if err != nil && held {
				t.Fatal("a failed probe reported the address as held")
			}
		})
	}
}

// The reason is carried in the error so the warning the operator sees says why
// the check could not be made.
func TestInterpretDuplicateProbeKeepsTheReason(t *testing.T) {
	_, _, err := interpretDuplicateProbe("arping: Interface \"ens35\" is down\n", 2, errors.New("exit status 2"))
	if err == nil || !strings.Contains(err.Error(), "is down") {
		t.Fatalf("err = %v, want it to carry arping's own message", err)
	}
}

// Nothing to ask about on an interface that is not there: the add itself
// reports the missing interface, and a probe error on top would bury it.
func TestAddressHeldElsewhereSkipsAMissingInterface(t *testing.T) {
	holder, held, err := AddressHeldElsewhere("pulseha-no-such-interface", "172.16.73.43/24")
	if err != nil || held || holder != "" {
		t.Fatalf("holder=%q held=%v err=%v, want nothing to report", holder, held, err)
	}
}
