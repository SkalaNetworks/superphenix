// Package quota holds the quota resource model of the Superphenix API, which
// stores and validates quotas, then translates them into the Kubernetes
// ResourceQuota `spec.hard` entries sent to the controller of each AZ.
//
// A quota is a flat map of resource name to quantity. Three abstract resources
// are translated into several Kubernetes quota keys; every other key is passed
// through to Kubernetes untouched (e.g. "pods", "count/virtualmachines.kubevirt.io").
//
// Semantics: a resource absent from the map is not limited by this level, a
// resource present with "0" is limited to zero.
package quota

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Abstract resources, translated by ToKubernetesQuota.
const (
	CPU    = "cpu"
	Memory = "memory"
	Disk   = "disk"
)

// MaxResources bounds the number of resources of a single quota.
const MaxResources = 64

var ErrInvalidResources = errors.New("invalid quota resources")

// translations maps each abstract resource to the Kubernetes quota keys it produces.
var translations = map[string][]string{
	CPU:    {"requests.cpu", "limits.cpu"},
	Memory: {"requests.memory", "limits.memory"},
	Disk:   {"requests.storage"},
}

// reserved holds the Kubernetes keys produced by a translation. They cannot be
// set directly, otherwise they would silently conflict with the abstract resource.
var reserved = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, keys := range translations {
		for _, k := range keys {
			m[k] = struct{}{}
		}
	}
	return m
}()

// Resources is the abstract quota of an organization, a project or a project AZ.
type Resources map[string]string

// Quantities is the canonical numeric form of Resources: CPU in millicores,
// every other resource in its base unit (rounded up).
type Quantities map[string]int64

// Parse validates and converts Resources into Quantities.
func (r Resources) Parse() (Quantities, error) {
	if len(r) > MaxResources {
		return nil, fmt.Errorf("%w: at most %d resources are allowed", ErrInvalidResources, MaxResources)
	}
	q := make(Quantities, len(r))
	for name, value := range r {
		if err := validateName(name); err != nil {
			return nil, err
		}
		v, err := parseQuantity(name, value)
		if err != nil {
			return nil, err
		}
		q[name] = v
	}
	return q, nil
}

func validateName(name string) error {
	if _, ok := reserved[name]; ok {
		return fmt.Errorf("%w: %q is managed by the %q resource and cannot be set directly", ErrInvalidResources, name, abstractOf(name))
	}
	if _, ok := translations[name]; ok {
		return nil
	}
	if errs := validation.IsQualifiedName(name); len(errs) > 0 {
		return fmt.Errorf("%w: invalid resource name %q: %s", ErrInvalidResources, name, strings.Join(errs, "; "))
	}
	return nil
}

func abstractOf(k8sKey string) string {
	for abstract, keys := range translations {
		if slices.Contains(keys, k8sKey) {
			return abstract
		}
	}
	return ""
}

func parseQuantity(name, value string) (int64, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid %s %q: %v", ErrInvalidResources, name, value, err)
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("%w: %s cannot be negative", ErrInvalidResources, name)
	}
	limit := resource.NewQuantity(math.MaxInt64, resource.DecimalSI)
	if name == CPU {
		limit = resource.NewMilliQuantity(math.MaxInt64, resource.DecimalSI)
	}
	// ParseQuantity clamps huge values to the limit, so reaching it means too large.
	if q.Cmp(*limit) >= 0 {
		return 0, fmt.Errorf("%w: %s %q is too large", ErrInvalidResources, name, value)
	}
	if name == CPU {
		return q.MilliValue(), nil
	}
	return q.Value(), nil
}

// ToKubernetesQuota translates the abstract resources into ResourceQuota `spec.hard` keys.
func (r Resources) ToKubernetesQuota() map[string]string {
	hard := make(map[string]string, len(r))
	for name, value := range r {
		if keys, ok := translations[name]; ok {
			for _, k := range keys {
				hard[k] = value
			}
			continue
		}
		hard[name] = value
	}
	return hard
}

// Resources formats Quantities back into Resources.
func (q Quantities) Resources() Resources {
	r := make(Resources, len(q))
	for name, v := range q {
		switch name {
		case CPU:
			if v%1000 == 0 {
				r[name] = fmt.Sprintf("%d", v/1000)
			} else {
				r[name] = fmt.Sprintf("%dm", v)
			}
		case Memory, Disk:
			r[name] = resource.NewQuantity(v, resource.BinarySI).String()
		default:
			r[name] = fmt.Sprintf("%d", v)
		}
	}
	return r
}

// Add returns the per-resource sum of q and other (union of resources, saturating on overflow).
func (q Quantities) Add(other Quantities) Quantities {
	res := make(Quantities, len(q)+len(other))
	for name, v := range q {
		res[name] = v
	}
	for name, v := range other {
		res[name] = saturatingAdd(res[name], v)
	}
	return res
}

// Sub returns q minus other for the resources of q, clamped to 0.
func (q Quantities) Sub(other Quantities) Quantities {
	res := make(Quantities, len(q))
	for name, v := range q {
		res[name] = max(v-other[name], 0)
	}
	return res
}

// SplitEqually divides every resource into count parts (floor division, so the
// sum of the parts never exceeds q). A resource is kept even when its part is 0,
// so that a limited resource never becomes unlimited after a split.
func (q Quantities) SplitEqually(count int) Quantities {
	if count <= 0 {
		return Quantities{}
	}
	res := make(Quantities, len(q))
	for name, v := range q {
		res[name] = v / int64(count)
	}
	return res
}

// Exceeds reports the first resource limited by limit for which q is greater.
// A resource missing from q counts as 0.
func (q Quantities) Exceeds(limit Quantities) (string, bool) {
	for _, name := range sortedNames(limit) {
		if q[name] > limit[name] {
			return fmt.Sprintf("%s quota exceeded: requested %s exceeds limit %s",
				name, q.format(name), limit.format(name)), true
		}
	}
	return "", false
}

// NotIn returns the resources of q that are absent from other, sorted.
func (q Quantities) NotIn(other Quantities) []string {
	var names []string
	for _, name := range sortedNames(q) {
		if _, ok := other[name]; !ok {
			names = append(names, name)
		}
	}
	return names
}

func (q Quantities) format(name string) string {
	return Quantities{name: q[name]}.Resources()[name]
}

func sortedNames(q Quantities) []string {
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// EffectiveAZQuota computes the resources to enforce in one AZ of a project,
// given the quota of each level (nil when the level has no quota).
//
// It fails closed: when an ancestor (organization or project) limits a
// resource, that resource is always enforced in the AZ, at 0 when the AZ has
// no allocation for it (e.g. a project created, or an AZ added, after the
// quotas were distributed). Resources no ancestor limits are ignored, so
// tenants cannot override platform defaults on resources they were not given.
// Without any ancestor quota, nothing is enforced (an AZ quota cannot exist
// without a project quota).
func EffectiveAZQuota(org, project, az Resources) Resources {
	res := make(Resources, len(org)+len(project))
	for _, parent := range []Resources{org, project} {
		for name := range parent {
			if v, ok := az[name]; ok {
				res[name] = v
			} else {
				res[name] = "0"
			}
		}
	}
	return res
}
