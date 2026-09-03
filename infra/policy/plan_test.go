//go:build tfplan

package policy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The resource count is a literal a reviewer has to change deliberately. A
// resource added without noticing is then a failing test with a number in it,
// which is the cheapest guard there is against a hand-edited stack.
const plannedResources = 45

func TestNoSecurityGroupIngressIsOpenToTheWorld(t *testing.T) {
	p := load(t)
	allowed := p.stringList(t, "alb_allowed_cidrs")

	groups := p.resources("aws_security_group")
	if len(groups) == 0 {
		t.Fatal("the plan has no security groups; this assertion would be vacuous")
	}
	for _, g := range groups {
		for _, r := range p.rules(t, g.Address) {
			if r.Direction != "ingress" {
				continue
			}
			// The load balancer is the one exception, and it is a written
			// answer rather than a silence: its CIDRs must be exactly the
			// operator's declared list, which has no default. A hardcoded
			// 0.0.0.0/0 here is caught even though 0.0.0.0/0 is a legitimate
			// value for that variable.
			if g.Address == "aws_security_group.alb" {
				if !slices.Equal(r.CIDRs, allowed) {
					t.Errorf("%s cidr_blocks are not var.alb_allowed_cidrs; the load balancer's reach must be the operator's declared answer", r)
				}
				continue
			}
			if r.OpenToTheWorld() {
				t.Errorf("%s is open to 0.0.0.0/0", r)
			}
		}
	}
}

func TestEverySecurityGroupBelongsToThisStacksVPC(t *testing.T) {
	p := load(t)
	for _, g := range p.resources("aws_security_group") {
		c, ok := p.config(g.Address)
		if !ok {
			t.Fatalf("%s has no configuration entry", g.Address)
		}
		// By reference and not by value: vpc_id is unknown at plan time, so a
		// group pointed at a VPC this stack does not create would look
		// identical in planned_values.
		if refs := c.references("vpc_id"); !slices.Equal(refs, []string{"aws_vpc.main"}) {
			t.Errorf("%s vpc_id references %v, want [aws_vpc.main]", g.Address, refs)
		}
	}
}

// The single decision that keeps every assertion in this package runnable with
// no AWS account. One data source that calls the API and terraform plan needs
// credentials, and this whole suite becomes account-required.
func TestThePlanContainsNoDataSourceThatCallsTheAWSAPI(t *testing.T) {
	p := load(t)
	// One written exception: the provider renders an IAM policy document
	// locally and makes no API call for it.
	local := []string{"aws_iam_policy_document"}
	for _, c := range p.Configuration.RootModule.Resources {
		if c.Mode != "data" || slices.Contains(local, c.Type) {
			continue
		}
		t.Errorf("%s is a data source that calls the AWS API; plan would need credentials and this suite would need an account", c.Address)
	}
}

func TestThePlanIsCreateOnlyFromEmptyState(t *testing.T) {
	p := load(t)
	var create, other int
	for _, c := range p.ResourceChanges {
		// A data source is read at plan time, not created. The only one here is
		// aws_iam_policy_document, which the provider renders locally;
		// TestThePlanContainsNoDataSourceThatCallsTheAWSAPI is what keeps that
		// true.
		if c.Mode == "data" {
			continue
		}
		switch {
		case slices.Equal(c.Change.Actions, []string{"create"}):
			create++
		case slices.Equal(c.Change.Actions, []string{"no-op"}):
		default:
			other++
			t.Errorf("%s plans %v; the offline plan runs on empty state and can only create", c.Address, c.Change.Actions)
		}
	}
	if create != plannedResources {
		t.Errorf("the plan creates %d resources, want %d — update the literal deliberately if this is intended", create, plannedResources)
	}
	if other != 0 {
		t.Errorf("%d resources plan something other than a create", other)
	}
}

// The offline half of the reproducibility claim: it catches a random_* resource,
// a timestamp() or a uuid() that would make every plan a diff. The other half —
// apply, plan-is-empty, destroy, apply, plan-is-empty — needs an account and is
// in the ledger.
func TestTwoPlansOfTheSameConfigurationAreIdentical(t *testing.T) {
	if os.Getenv("CODETRAIL_TFPLAN_JSON") != "" {
		t.Skip("a counterfactual has pointed the suite at a specific plan")
	}
	first, err := os.ReadFile(planPath())
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "terraform", "offline_plan.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("re-planning failed: %v\n%s", err, out)
	}
	second, err := os.ReadFile(planPath())
	if err != nil {
		t.Fatal(err)
	}
	a, b := canonical(t, first), canonical(t, second)
	if a != b {
		t.Error("two plans of the same configuration differ; something in the stack is not deterministic")
	}
}

// canonical is the plan reduced to what this configuration actually decides.
//
// `terraform show -json` is NOT byte-stable across two plans of one
// configuration on terraform 1.9.8, in three places, all measured rather than
// assumed:
//
//   - timestamp is when show ran.
//   - relevant_attributes, which records which attributes the plan depends on
//     and which nothing here reads, comes back reordered.
//   - configuration.*.expressions.*.references comes back reordered — three
//     consecutive plans gave three different orders for one ingress block. That
//     one is inside a section these assertions DO read, so it is normalised by
//     sorting rather than dropped.
//
// Everything else is compared in full, so a fourth non-deterministic field is a
// failure rather than an exemption.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "timestamp")
	delete(m, "relevant_attributes")
	sortReferences(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func sortReferences(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == "references" {
				if list, ok := child.([]any); ok {
					sort.Slice(list, func(i, j int) bool {
						a, _ := list[i].(string)
						b, _ := list[j].(string)
						return a < b
					})
					continue
				}
			}
			sortReferences(child)
		}
	case []any:
		for _, child := range t {
			sortReferences(child)
		}
	}
}

// Three artifacts hold this stack's secrets in cleartext: the plan file, the
// JSON show produces from it, and state. None of them may ever be committed.
func TestNoStateOrTfvarsFileIsTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--", ":/").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	forbidden := []string{".tfstate", ".tfvars", "tfplan", ".terraform/", "backend.hcl"}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		for _, bad := range forbidden {
			// backend.hcl.example is the committed template and is the whole
			// point of having one.
			if strings.HasSuffix(f, ".example") {
				continue
			}
			if strings.Contains(f, bad) {
				t.Errorf("%s is tracked and matches %q; it can hold the database password in cleartext", f, bad)
			}
		}
	}
}
