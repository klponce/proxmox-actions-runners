package vmtags

import (
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

func TestTagValues(t *testing.T) {
	vm := proxmox.VM{Tags: []string{Managed, Template, "par-tv-1790000000", "par-rv-2.337.0", "par-tv-bogus"}}
	if n, ok := Int(vm, TemplateVersionPrefix); !ok || n != 1790000000 {
		t.Errorf("Int = %d, %v", n, ok)
	}
	if s, ok := String(vm, RunnerVersionPrefix); !ok || s != "2.337.0" {
		t.Errorf("String = %q, %v", s, ok)
	}
	if _, ok := Int(vm, TemplateRefPrefix); ok {
		t.Error("Int found a missing tag")
	}
	if _, ok := Int(proxmox.VM{Tags: []string{"par-tv-"}}, TemplateVersionPrefix); ok {
		t.Error("Int accepted an empty value")
	}
	if _, ok := Int(proxmox.VM{Tags: []string{"par-tv-x"}}, TemplateVersionPrefix); ok {
		t.Error("Int accepted a non-number")
	}
}

func TestNewestTemplate(t *testing.T) {
	tpl := func(vmid int, pool string, template bool, tags ...string) proxmox.VM {
		return proxmox.VM{VMID: vmid, Pool: pool, Template: template, Tags: tags}
	}
	vms := []proxmox.VM{
		tpl(9000, "par-runners", true, Managed, Template, "par-tv-100"),
		tpl(9001, "par-runners", true, Managed, Template, "par-tv-300"),
		tpl(9002, "par-runners", true, Managed, Template, "par-tv-300"),  // tie: higher VMID wins
		tpl(9003, "par-runners", true, Managed, Template),                // no version
		tpl(9004, "par-runners", true, Template, "par-tv-999"),           // not managed
		tpl(9005, "other", true, Managed, Template, "par-tv-999"),        // another pool
		tpl(9006, "par-runners", false, Managed, Template, "par-tv-999"), // not a Proxmox template (yet)
	}
	got, ok := NewestTemplate(vms, "par-runners")
	if !ok || got.VMID != 9002 {
		t.Errorf("NewestTemplate = %d, %v; want 9002", got.VMID, ok)
	}
	if _, ok := NewestTemplate(vms[3:], "par-runners"); ok {
		t.Error("NewestTemplate found one among templates it must ignore")
	}
}
