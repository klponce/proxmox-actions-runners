package settings

import (
	"errors"
	"strings"
	"testing"
)

var testLimits = Limits{HostCPUs: 8, HostMemMiB: 63488}

func TestKeysAreSortedAndComplete(t *testing.T) {
	for i, k := range Keys {
		if i > 0 && Keys[i-1].Name >= k.Name {
			t.Errorf("%s comes after %s", k.Name, Keys[i-1].Name)
		}
		if k.Summary == "" || k.Default == "" || k.Applies == "" || k.Allowed == nil || k.Get == nil || k.Set == nil {
			t.Errorf("%s is incomplete", k.Name)
		}
	}
}

func TestSetKey(t *testing.T) {
	tests := []struct {
		key, value string
		want       string // the value Get returns afterwards
		wantErr    string
	}{
		{key: "runners.max", value: "4", want: "4"},
		{key: "runners.max", value: " 96 ", want: "96"},
		{key: "runners.max", value: "97", wantErr: "must be 1 to 96"},
		{key: "runners.max", value: "0", wantErr: "must be 1 to 96"},
		{key: "runners.max", value: "two", wantErr: "not a whole number"},
		{key: "worker.cores", value: "4", want: "4"},
		{key: "worker.cores", value: "9", wantErr: "this node has 8 CPU threads"},
		{key: "worker.cores", value: "0", wantErr: "must be at least 1"},
		{key: "worker.memory", value: "16GiB", want: "16GiB"},
		{key: "worker.memory", value: "12288MiB", want: "12GiB"},
		{key: "worker.memory", value: "8704MiB", want: "8704MiB"},
		{key: "worker.memory", value: "16GB", wantErr: "use GiB or MiB"},
		{key: "worker.memory", value: "512MiB", wantErr: "must be at least 1GiB"},
		{key: "worker.memory", value: "64GiB", wantErr: "this node has only 62GiB"},
		{key: "worker.disk", value: "20GiB", wantErr: "unknown key"},
	}
	for _, tt := range tests {
		s := Default()
		err := SetKey(&s, tt.key, tt.value, testLimits)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("set %s %q = %v, want %q", tt.key, tt.value, err, tt.wantErr)
			}
			if d := Default(); s.ScaleSet.MaxRunners != d.ScaleSet.MaxRunners || s.Worker != d.Worker {
				t.Errorf("set %s %q changed the settings on error", tt.key, tt.value)
			}
			continue
		}
		if err != nil {
			t.Errorf("set %s %q: %v", tt.key, tt.value, err)
			continue
		}
		k, _ := Lookup(tt.key)
		if got, _ := k.Get(&s); got != tt.want {
			t.Errorf("after set %s %q, get = %q, want %q", tt.key, tt.value, got, tt.want)
		}
	}
}

func TestSetKeyChecksTheWholeSettings(t *testing.T) {
	s := Default()
	s.ScaleSet.MinRunners = 3
	s.ScaleSet.MaxRunners = 5
	var verr *ValueError
	if err := SetKey(&s, "runners.max", "2", testLimits); !errors.As(err, &verr) || !strings.Contains(err.Error(), "3 to 96") {
		t.Errorf("err = %v", err)
	}
	k, _ := Lookup("runners.max")
	if allowed := k.Allowed(&s, testLimits); !strings.Contains(allowed, "less than minRunners (3)") {
		t.Errorf("Allowed = %q", allowed)
	}
}

func TestGetShowsDefaults(t *testing.T) {
	s := Default()
	for name, want := range map[string]string{"runners.max": "1", "worker.cores": "2", "worker.memory": "8GiB"} {
		k, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if got, isDefault := k.Get(&s); got != want || !isDefault {
			t.Errorf("%s = %q (default %v), want %q", name, got, isDefault, want)
		}
	}
}

func TestWarnings(t *testing.T) {
	s := Default()
	s.ScaleSet.MaxRunners = 8
	w := Warnings(&s, testLimits)
	if len(w) != 1 || !strings.Contains(w[0].Text, "64GiB, more than this node's 62GiB") {
		t.Fatalf("Warnings = %q", w)
	}
	// It is about the two keys that make it up, so setting either shows it, and setting another doesn't.
	if !w[0].About("runners.max") || !w[0].About("worker.memory") || w[0].About("worker.cores") {
		t.Errorf("warning keys = %v", w[0].Keys)
	}
	s.ScaleSet.MaxRunners = 7
	if w := Warnings(&s, testLimits); len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}

func TestDescribe(t *testing.T) {
	s := Default()
	s.Worker.MemoryMiB = 16384
	k, _ := Lookup("worker.memory")
	want := `worker.memory  memory per worker VM
  allowed:  a size in GiB or MiB from 1GiB to 62GiB (this node's memory), such as 8GiB or 12288MiB; GB and MB aren't accepted, because Proxmox sizes memory in binary units
  default:  8GiB, like GitHub's ubuntu-latest for private repositories
  current:  16GiB
  applies:  to new workers; the controller restarts, and running workers keep going
`
	if got := Describe(k, &s, testLimits); got != want {
		t.Errorf("Describe =\n%s\nwant\n%s", got, want)
	}
}
