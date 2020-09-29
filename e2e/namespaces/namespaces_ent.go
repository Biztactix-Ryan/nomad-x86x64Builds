// +build ent

package namespaces

import (
	"fmt"
	"os"

	e2e "github.com/hashicorp/nomad/e2e/e2eutil"
	"github.com/hashicorp/nomad/e2e/framework"
	"github.com/hashicorp/nomad/helper/uuid"
)

type NamespacesE2ETest struct {
	framework.TC
	namespaceIDs     []string
	namespacedJobIDs [][2]string // [(ns, jobID)]
}

func init() {
	framework.AddSuites(&framework.TestSuite{
		Component:   "Namespaces",
		CanRunLocal: true,
		Consul:      true,
		Cases: []framework.TestCase{
			new(NamespacesE2ETest),
		},
	})

}

func (tc *NamespacesE2ETest) BeforeAll(f *framework.F) {
	e2e.WaitForLeader(f.T(), tc.Nomad())
	e2e.WaitForNodesReady(f.T(), tc.Nomad(), 1)
}

func (tc *NamespacesE2ETest) AfterEach(f *framework.F) {
	if os.Getenv("NOMAD_TEST_SKIPCLEANUP") == "1" {
		return
	}
	var err error

	for _, pair := range tc.namespacedJobIDs {
		ns := pair[0]
		jobID := pair[1]
		if ns != "" {
			_, err = e2e.Command("nomad", "job", "stop", "-purge", "-namespace", ns, jobID)
		} else {
			_, err = e2e.Command("nomad", "job", "stop", "-purge", jobID)
		}
		f.NoError(err)
	}

	for _, ns := range tc.namespaceIDs {
		_, err = e2e.Command("nomad", "namespace", "delete", ns)
		f.NoError(err)
	}
	tc.namespaceIDs = []string{}

	_, err = e2e.Command("nomad", "system", "gc")
	f.NoError(err)
}

// TestNamespacesFiltering exercises the -namespace flag on various commands
// to ensure that they are properly isolated
func (tc *NamespacesE2ETest) TestNamespacesFiltering(f *framework.F) {

	_, err := e2e.Command("nomad", "namespace", "apply",
		"-description", "namespace A", "NamespaceA")
	f.NoError(err, "could not create namespace")
	tc.namespaceIDs = append(tc.namespaceIDs, "NamespaceA")

	_, err = e2e.Command("nomad", "namespace", "apply",
		"-description", "namespace B", "NamespaceB")
	f.NoError(err, "could not create namespace")
	tc.namespaceIDs = append(tc.namespaceIDs, "NamespaceB")

	run := func(jobspec, ns string) string {
		jobID := "test-namespace-" + uuid.Generate()[0:8]
		f.NoError(e2e.Register(jobID, jobspec))
		tc.namespacedJobIDs = append(tc.namespacedJobIDs, [2]string{ns, jobID})
		expected := []string{"running"}
		f.NoError(e2e.WaitForAllocStatusExpected(jobID, ns, expected), "job should be running")
		return jobID
	}

	jobA := run("namespaces/input/namespace_a.nomad", "NamespaceA")
	jobB := run("namespaces/input/namespace_b.nomad", "NamespaceB")
	jobDefault := run("namespaces/input/namespace_default.nomad", "")

	// exercise 'nomad job status' filtering

	out, err := e2e.Command("nomad", "job", "status", "-namespace", "NamespaceA")
	rows, err := e2e.ParseColumns(out)
	f.NoError(err, "could not parse job status output")
	f.Equal(1, len(rows))
	f.Equal(jobA, rows[0]["ID"])

	out, err = e2e.Command("nomad", "job", "status", "-namespace", "NamespaceB")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse job status output")
	f.Equal(1, len(rows))
	f.Equal(jobB, rows[0]["ID"])

	out, err = e2e.Command("nomad", "job", "status", "-namespace", "*")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse job status output")
	f.Equal(3, len(rows))

	out, err = e2e.Command("nomad", "job", "status")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse job status output")
	f.Equal(1, len(rows))
	f.Equal(jobDefault, rows[0]["ID"])

	// exercise 'nomad status' filtering

	out, err = e2e.Command("nomad", "status", "-namespace", "NamespaceA")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse status output")
	f.Equal(1, len(rows))
	f.Equal(jobA, rows[0]["ID"])

	out, err = e2e.Command("nomad", "status", "-namespace", "NamespaceB")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse status output")
	f.Equal(1, len(rows))
	f.Equal(jobB, rows[0]["ID"])

	out, err = e2e.Command("nomad", "status", "-namespace", "*")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse status output")
	f.Equal(3, len(rows))

	out, err = e2e.Command("nomad", "status")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse status output")
	f.Equal(1, len(rows))
	f.Equal(jobDefault, rows[0]["ID"])

	// exercise 'nomad deployment list' filtering
	// note: '-namespace *' is only supported for job and alloc subcommands

	out, err = e2e.Command("nomad", "deployment", "list", "-namespace", "NamespaceA")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse deployment list output")
	f.Equal(1, len(rows))
	f.Equal(jobA, rows[0]["Job ID"])

	out, err = e2e.Command("nomad", "deployment", "list", "-namespace", "NamespaceB")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse deployment list output")
	f.Equal(len(rows), 1)
	f.Equal(jobB, rows[0]["Job ID"])

	out, err = e2e.Command("nomad", "deployment", "list")
	rows, err = e2e.ParseColumns(out)
	f.NoError(err, "could not parse deployment list output")
	f.Equal(1, len(rows))
	f.Equal(jobDefault, rows[0]["Job ID"])

	out, err = e2e.Command("nomad", "job", "stop", jobA)
	f.Equal(fmt.Sprintf("No job(s) with prefix or id %q found\n", jobA), out)
	f.Error(err, "exit status 1")

	_, err = e2e.Command("nomad", "job", "stop", "-namespace", "NamespaceA", jobA)
	f.NoError(err, "could not stop job in namespace")
}
