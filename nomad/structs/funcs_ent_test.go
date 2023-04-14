//go:build ent
// +build ent

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestUpdateUsageFromPlan(t *testing.T) {
	ci.Parallel(t)

	// Create a quota usage that has some amount set
	usage := &QuotaUsage{
		Name: "test quota",
		Used: map[string]*QuotaLimit{
			"foo": {
				Region: "global",
				RegionLimit: &Resources{
					CPU:         2000,
					MemoryMB:    1000,
					MemoryMaxMB: 3000,
				},
			},
		},
	}

	nodeID := "123"
	plan := &Plan{
		NodeUpdate:     make(map[string][]*Allocation),
		NodeAllocation: make(map[string][]*Allocation),
	}
	plan.NodeUpdate[nodeID] = make([]*Allocation, 0, 1)
	plan.NodeAllocation[nodeID] = make([]*Allocation, 0, 2)

	// Create an allocation - Should add
	add := &Allocation{
		TaskResources: map[string]*Resources{
			"web": {
				CPU:         101,
				MemoryMB:    202,
				MemoryMaxMB: 202,
			},
			"web 2": {
				CPU:      303,
				MemoryMB: 404,
			},
		},
	}
	plan.NodeAllocation[nodeID] = append(plan.NodeAllocation[nodeID], add)

	// Inplace update an allocation - Should be ignored
	ignore := &Allocation{
		CreateIndex: 100,
		TaskResources: map[string]*Resources{
			"web": {
				CPU:      111,
				MemoryMB: 222,
			},
			"web 2": {
				CPU:      333,
				MemoryMB: 444,
			},
		},
	}
	plan.NodeAllocation[nodeID] = append(plan.NodeAllocation[nodeID], ignore)

	// Remove an allocation - Should be discounted
	rm := &Allocation{
		ClientStatus:  AllocClientStatusRunning,
		DesiredStatus: AllocDesiredStatusStop,
		TaskResources: map[string]*Resources{
			"web": {
				CPU:      110,
				MemoryMB: 220,
			},
			"web 2": {
				CPU:      330,
				MemoryMB: 440,
			},
		},
	}
	plan.NodeUpdate[nodeID] = append(plan.NodeUpdate[nodeID], rm)

	effected := UpdateUsageFromPlan(usage, plan)
	must.Len(t, 1, effected)

	expected := &QuotaLimit{
		Region: "global",
		RegionLimit: &Resources{
			CPU:         1964,
			MemoryMB:    946,
			MemoryMaxMB: 2946,
		},
	}
	must.Eq(t, expected, effected[0])
}
