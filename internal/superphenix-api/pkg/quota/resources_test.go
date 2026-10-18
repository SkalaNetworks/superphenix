package quota

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gi = int64(1024 * 1024 * 1024)

func TestParse(t *testing.T) {
	t.Run("abstract and arbitrary resources", func(t *testing.T) {
		q, err := Resources{
			CPU:                                 "2500m",
			Memory:                              "64Gi",
			Disk:                                "1Ti",
			"pods":                              "50",
			"count/virtualmachines.kubevirt.io": "20",
			"gold.storageclass.storage.k8s.io/requests.storage": "100Gi",
			"requests.nvidia.com/gpu":                           "0",
		}.Parse()
		require.NoError(t, err)
		assert.Equal(t, Quantities{
			CPU:                                 2500,
			Memory:                              64 * gi,
			Disk:                                1024 * gi,
			"pods":                              50,
			"count/virtualmachines.kubevirt.io": 20,
			"gold.storageclass.storage.k8s.io/requests.storage": 100 * gi,
			"requests.nvidia.com/gpu":                           0,
		}, q)
	})

	invalid := map[string]Resources{
		"invalid quantity":         {CPU: "invalid"},
		"negative quantity":        {"pods": "-1"},
		"too large quantity":       {Memory: "100Ei"},
		"too large cpu":            {CPU: "10E15"},
		"empty quantity":           {CPU: ""},
		"translated key":           {"requests.cpu": "4"},
		"translated storage key":   {"requests.storage": "4Gi"},
		"yaml injection in name":   {"pods: 1\n  limits.cpu": "1"},
		"invalid name":             {"count/": "1"},
		"name with spaces":         {"my pods": "1"},
		"name with leading slash":  {"/pods": "1"},
		"name with invalid prefix": {"Count_/pods": "1"},
	}
	for name, r := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := r.Parse()
			assert.ErrorIs(t, err, ErrInvalidResources)
		})
	}

	t.Run("too many resources", func(t *testing.T) {
		r := Resources{}
		for i := 0; i <= MaxResources; i++ {
			r["count/r"+strings.Repeat("x", i)] = "1"
		}
		_, err := r.Parse()
		assert.ErrorIs(t, err, ErrInvalidResources)
	})
}

func TestToKubernetesQuota(t *testing.T) {
	hard := Resources{
		CPU:                                 "16",
		Memory:                              "64Gi",
		Disk:                                "1Ti",
		"pods":                              "50",
		"count/virtualmachines.kubevirt.io": "0",
	}.ToKubernetesQuota()
	assert.Equal(t, map[string]string{
		"requests.cpu":                      "16",
		"limits.cpu":                        "16",
		"requests.memory":                   "64Gi",
		"limits.memory":                     "64Gi",
		"requests.storage":                  "1Ti",
		"pods":                              "50",
		"count/virtualmachines.kubevirt.io": "0",
	}, hard)
}

func TestQuantitiesResources(t *testing.T) {
	assert.Equal(t, Resources{CPU: "16", Memory: "64Gi", Disk: "1Ti", "pods": "0"},
		Quantities{CPU: 16000, Memory: 64 * gi, Disk: 1024 * gi, "pods": 0}.Resources())
	assert.Equal(t, Resources{CPU: "3500m", Memory: "500Mi"},
		Quantities{CPU: 3500, Memory: 500 * 1024 * 1024}.Resources())
}

func TestSplitEqually(t *testing.T) {
	orig := Quantities{CPU: 10000, Memory: 300 * gi, "count/virtualmachines.kubevirt.io": 2}

	split := orig.SplitEqually(3)
	assert.Equal(t, Quantities{CPU: 3333, Memory: 100 * gi, "count/virtualmachines.kubevirt.io": 0}, split)
	_, exceeded := split.Add(split).Add(split).Exceeds(orig)
	assert.False(t, exceeded, "the parts never exceed the whole")

	// A resource split down to 0 stays limited (at 0), it does not become unlimited.
	assert.Equal(t, "0", split.Resources()["count/virtualmachines.kubevirt.io"])

	assert.Empty(t, orig.SplitEqually(0))
	assert.Empty(t, orig.SplitEqually(-1))
}

func TestAddSub(t *testing.T) {
	a := Quantities{CPU: 1000, "pods": 10}
	b := Quantities{CPU: 500, Memory: gi}
	assert.Equal(t, Quantities{CPU: 1500, Memory: gi, "pods": 10}, a.Add(b))
	assert.Equal(t, Quantities{CPU: 500, "pods": 10}, a.Sub(b))
	assert.Equal(t, Quantities{CPU: 0, Memory: gi}, b.Sub(Quantities{CPU: 2000}))

	// Sums saturate instead of overflowing into negative (i.e. passing) values.
	huge := Quantities{"pods": 1 << 62}
	assert.Equal(t, int64(1<<63-1), huge.Add(huge).Add(huge)["pods"])
}

func TestExceeds(t *testing.T) {
	limit := Quantities{CPU: 16000, Memory: 64 * gi, "count/virtualmachines.kubevirt.io": 0}

	_, exceeded := Quantities{CPU: 8000, Memory: 64 * gi}.Exceeds(limit)
	assert.False(t, exceeded, "missing resources count as 0, equal is allowed")

	_, exceeded = Quantities{"pods": 1000}.Exceeds(limit)
	assert.False(t, exceeded, "resources not limited are ignored")

	msg, exceeded := Quantities{CPU: 17000}.Exceeds(limit)
	assert.True(t, exceeded)
	assert.Equal(t, "cpu quota exceeded: requested 17 exceeds limit 16", msg)

	msg, exceeded = Quantities{"count/virtualmachines.kubevirt.io": 1}.Exceeds(limit)
	assert.True(t, exceeded, "a resource limited to 0 is enforced")
	assert.Contains(t, msg, "count/virtualmachines.kubevirt.io")
}

func TestNotIn(t *testing.T) {
	assert.Equal(t, []string{"b", "c"}, Quantities{"a": 1, "c": 1, "b": 1}.NotIn(Quantities{"a": 0}))
	assert.Empty(t, Quantities{"a": 1}.NotIn(Quantities{"a": 0, "b": 0}))
}

func TestEffectiveAZQuota(t *testing.T) {
	az := Resources{CPU: "4", "pods": "10"}

	assert.Empty(t, EffectiveAZQuota(nil, nil, az), "an AZ quota is only enforced within an ancestor quota")

	assert.Equal(t, Resources{CPU: "4", "pods": "10"},
		EffectiveAZQuota(nil, Resources{CPU: "8", "pods": "20"}, az))

	assert.Equal(t, Resources{CPU: "4", Memory: "0"},
		EffectiveAZQuota(nil, Resources{CPU: "8", Memory: "8Gi"}, az),
		"resources limited by the project but not allocated to the AZ are 0, the others are ignored")

	assert.Equal(t, Resources{CPU: "0", Memory: "0"},
		EffectiveAZQuota(Resources{CPU: "8", Memory: "8Gi"}, nil, nil),
		"a project without quota in a limited organization gets nothing")

	assert.Equal(t, Resources{CPU: "4", "pods": "10", Memory: "0"},
		EffectiveAZQuota(Resources{Memory: "8Gi"}, Resources{CPU: "8", "pods": "20"}, az),
		"resources limited by the organization are enforced even if the project quota predates them")
}
