package settings

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/config"
)

// Limits are what the node allows, for checking a value. A zero field means unknown and isn't checked.
type Limits struct {
	HostCPUs   int
	HostMemMiB int
}

// Key is one setting users change with `parcon config set`. Adding a key means adding one of these to Keys.
type Key struct {
	Name string
	// Summary is one line on what the key is.
	Summary string
	// Allowed says which values the key takes on this node. `config describe` shows it, and so does `config set`
	// when a value is refused.
	Allowed func(*Settings, Limits) string
	// Default is the value when the key isn't set, and why.
	Default string
	// Get returns the key's value as `config set` takes it, and whether that is the default.
	Get func(*Settings) (value string, isDefault bool)
	// Set parses value and stores it. Its error says why a value is refused.
	Set func(s *Settings, value string, l Limits) error
	// Applies says when a change takes effect.
	Applies string
}

// ErrUnknownKey is a key that isn't in Keys.
var ErrUnknownKey = errors.New("unknown key")

// Keys are the settings users can change, sorted by name.
var Keys = []Key{runnersMax, workerCores, workerMemory}

// Lookup finds a key by name.
func Lookup(name string) (Key, error) {
	for _, k := range Keys {
		if k.Name == name {
			return k, nil
		}
	}
	names := make([]string, len(Keys))
	for i, k := range Keys {
		names[i] = k.Name
	}
	return Key{}, fmt.Errorf("%w %q: the keys are %s", ErrUnknownKey, name, strings.Join(names, ", "))
}

// ValueError is a value a key refuses.
type ValueError struct {
	Key    string
	Value  string
	Reason string
}

func (e *ValueError) Error() string {
	return fmt.Sprintf("%q isn't a valid %s: %s", e.Value, e.Key, e.Reason)
}

// SetKey sets a key and checks the whole settings afterwards, since a value can clash with another setting. On
// error, s is unchanged.
func SetKey(s *Settings, name, value string, l Limits) error {
	k, err := Lookup(name)
	if err != nil {
		return err
	}
	next := *s
	next.ScaleSet.Labels = slices.Clone(s.ScaleSet.Labels)
	if err := k.Set(&next, value, l); err != nil {
		return &ValueError{Key: name, Value: value, Reason: err.Error()}
	}
	if err := next.Validate(); err != nil {
		return &ValueError{Key: name, Value: value, Reason: err.Error()}
	}
	*s = next
	return nil
}

const appliesToNewWorkers = "to new workers; the controller restarts, and running workers keep going"

var runnersMax = Key{
	Name:    "runners.max",
	Summary: "the most worker VMs at once, and so the most jobs at once",
	Allowed: func(s *Settings, _ Limits) string {
		r := s.Proxmox.VMIDRange
		allowed := fmt.Sprintf("a whole number from %d to %d: the VMID range %d-%d has %d IDs for workers",
			max(1, s.ScaleSet.MinRunners), workerIDs(s), r.Start, r.End, workerIDs(s))
		if s.ScaleSet.MinRunners > 1 {
			allowed += fmt.Sprintf(", and it can't be less than minRunners (%d)", s.ScaleSet.MinRunners)
		}
		return allowed
	},
	Default: strconv.Itoa(config.DefaultMaxRunners),
	Get: func(s *Settings) (string, bool) {
		return strconv.Itoa(s.ScaleSet.MaxRunners), s.ScaleSet.MaxRunners == config.DefaultMaxRunners
	},
	Set: func(s *Settings, value string, _ Limits) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return errors.New("not a whole number")
		}
		if lo, hi := max(1, s.ScaleSet.MinRunners), workerIDs(s); n < lo || n > hi {
			return fmt.Errorf("must be %d to %d", lo, hi)
		}
		s.ScaleSet.MaxRunners = n
		return nil
	},
	Applies: "within seconds; the controller restarts, and running workers keep going",
}

// workerIDs is how many IDs of the VMID range are for workers.
func workerIDs(s *Settings) int { return max(s.Proxmox.VMIDRange.Size()-config.ReservedVMIDs, 0) }

var workerCores = Key{
	Name:    "worker.cores",
	Summary: "vCPUs per worker VM",
	Allowed: func(_ *Settings, l Limits) string {
		if l.HostCPUs > 0 {
			return fmt.Sprintf("a whole number from %d to %d (this node's CPU threads)", config.MinCores, l.HostCPUs)
		}
		return fmt.Sprintf("a whole number, at least %d and at most this node's CPU threads", config.MinCores)
	},
	Default: fmt.Sprintf("%d, like GitHub's ubuntu-latest for private repositories", config.DefaultCores),
	Get: func(s *Settings) (string, bool) {
		n := s.EffectiveWorker().Cores
		return strconv.Itoa(n), n == config.DefaultCores
	},
	Set: func(s *Settings, value string, l Limits) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return errors.New("not a whole number")
		}
		if n < config.MinCores {
			return fmt.Errorf("must be at least %d", config.MinCores)
		}
		if l.HostCPUs > 0 && n > l.HostCPUs {
			return fmt.Errorf("this node has %d CPU threads, and Proxmox won't start a VM with more", l.HostCPUs)
		}
		s.Worker.Cores = n
		return nil
	},
	Applies: appliesToNewWorkers,
}

var workerMemory = Key{
	Name:    "worker.memory",
	Summary: "memory per worker VM",
	Allowed: func(_ *Settings, l Limits) string {
		upper := "this node's memory"
		if l.HostMemMiB > 0 {
			upper = fmt.Sprintf("%s (this node's memory)", config.FormatMiB(l.HostMemMiB))
		}
		return fmt.Sprintf("a size in GiB or MiB from %s to %s, such as 8GiB or 12288MiB; GB and MB aren't "+
			"accepted, because Proxmox sizes memory in binary units", config.FormatMiB(config.MinMemoryMiB), upper)
	},
	Default: config.FormatMiB(config.DefaultMemoryMiB) + ", like GitHub's ubuntu-latest for private repositories",
	Get: func(s *Settings) (string, bool) {
		n := s.EffectiveWorker().MemoryMiB
		return config.FormatMiB(n), n == config.DefaultMemoryMiB
	},
	Set: func(s *Settings, value string, l Limits) error {
		mib, err := config.ParseSize(value)
		if err != nil {
			return err
		}
		if mib < config.MinMemoryMiB {
			return fmt.Errorf("must be at least %s", config.FormatMiB(config.MinMemoryMiB))
		}
		if l.HostMemMiB > 0 && mib > l.HostMemMiB {
			return fmt.Errorf("this node has only %s", config.FormatMiB(l.HostMemMiB))
		}
		s.Worker.MemoryMiB = mib
		return nil
	},
	Applies: appliesToNewWorkers,
}

// Warning is a combination of settings that is allowed but likely to cause trouble on this node.
type Warning struct {
	// Keys are the config keys the warning is about: changing any of them can cause or clear it.
	Keys []string
	Text string
}

// About reports whether the warning is about key.
func (w Warning) About(key string) bool { return slices.Contains(w.Keys, key) }

func (w Warning) String() string { return w.Text }

// Warnings are the settings that are allowed but likely to cause trouble on this node, such as more worker memory
// than the node has.
func Warnings(s *Settings, l Limits) []Warning {
	var w []Warning
	if l.HostMemMiB > 0 {
		if total := s.ScaleSet.MaxRunners * s.EffectiveWorker().MemoryMiB; total > l.HostMemMiB {
			w = append(w, Warning{Keys: []string{"runners.max", "worker.memory"}, Text: fmt.Sprintf(
				"runners.max %d × worker.memory %s is %s, more than this node's %s: workers will fail to start "+
					"once memory runs out", s.ScaleSet.MaxRunners, config.FormatMiB(s.EffectiveWorker().MemoryMiB),
				config.FormatMiB(total), config.FormatMiB(l.HostMemMiB))})
		}
	}
	return w
}

// Describe is a key's guidance: what it is, the values it takes on this node, its default, its current value, and
// when a change takes effect. `parcon config describe` prints it, and so does `parcon config set` for a refused value.
func Describe(k Key, s *Settings, l Limits) string {
	current, _ := k.Get(s)
	return fmt.Sprintf("%s  %s\n  allowed:  %s\n  default:  %s\n  current:  %s\n  applies:  %s\n", k.Name, k.Summary,
		k.Allowed(s, l), k.Default, current, k.Applies)
}
