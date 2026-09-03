//go:build tfplan

// Package policy reads `terraform show -json` output and asserts the things
// about this deployment that a comment in an HCL file cannot.
//
// Behind a build tag because it needs an artifact `go test ./...` cannot
// produce. It FAILS rather than skips when that artifact is absent: P2's review
// round found two live suites that emptied a developer's database while
// printing ok, and a policy suite that quietly passes because nobody ran
// terraform is the same defect wearing a different hat.
//
// encoding/json rather than hashicorp/terraform-json: `terraform show -json`
// has a documented, versioned schema, these assertions need three of its
// sections, and a dependency to read one map is a dependency to review forever.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// planPath is where offline_plan.sh writes. Overridable so a counterfactual can
// point the suite at a second plan without moving the first.
func planPath() string {
	if p := os.Getenv("CODETRAIL_TFPLAN_JSON"); p != "" {
		return p
	}
	return filepath.Join("..", "terraform", "tfplan.json")
}

// Plan is the part of the schema these assertions read.
//
// Three sections, because no one of them is enough:
//
//   - PlannedValues carries known attribute values: ports, protocols, CIDRs.
//   - ResourceChanges carries the actions, and AfterUnknown, which is the only
//     thing that distinguishes "this rule names no security group" from "this
//     rule names one whose id is not known until apply".
//   - Configuration carries the *references* an expression made, which is how a
//     rule scoped to aws_security_group.alb is asserted by name. Measured: a
//     security_groups field referencing another resource is omitted from
//     planned_values entirely and appears in after_unknown as true.
type Plan struct {
	FormatVersion string `json:"format_version"`
	Variables     map[string]struct {
		Value any `json:"value"`
	} `json:"variables"`
	PlannedValues struct {
		RootModule struct {
			Resources []Resource `json:"resources"`
		} `json:"root_module"`
	} `json:"planned_values"`
	ResourceChanges []ResourceChange `json:"resource_changes"`
	Configuration   struct {
		RootModule struct {
			Resources []ConfigResource `json:"resources"`
		} `json:"root_module"`
	} `json:"configuration"`
}

type Resource struct {
	Address string         `json:"address"`
	Mode    string         `json:"mode"`
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Index   any            `json:"index"`
	Values  map[string]any `json:"values"`
}

type ResourceChange struct {
	Address string `json:"address"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Change  struct {
		Actions      []string `json:"actions"`
		AfterUnknown any      `json:"after_unknown"`
	} `json:"change"`
}

type ConfigResource struct {
	Address     string         `json:"address"`
	Mode        string         `json:"mode"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Expressions map[string]any `json:"expressions"`
}

// load reads the plan, or fails the test naming the command that produces one.
func load(t *testing.T) *Plan {
	t.Helper()
	path := planPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no plan to assert over at %s: %v\n\nRun ./infra/terraform/offline_plan.sh — it needs no AWS account.", path, err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("%s is not a terraform plan: %v", path, err)
	}
	if len(p.ResourceChanges) == 0 {
		t.Fatalf("%s has no resource changes; every assertion below would be vacuous", path)
	}
	return &p
}

// resources returns every planned resource of a type.
func (p *Plan) resources(kind string) []Resource {
	var out []Resource
	for _, r := range p.PlannedValues.RootModule.Resources {
		if r.Type == kind && r.Mode == "managed" {
			out = append(out, r)
		}
	}
	return out
}

// resource returns the one planned resource at an address, or false.
func (p *Plan) resource(address string) (Resource, bool) {
	for _, r := range p.PlannedValues.RootModule.Resources {
		if r.Address == address {
			return r, true
		}
	}
	return Resource{}, false
}

func (p *Plan) change(address string) (ResourceChange, bool) {
	for _, c := range p.ResourceChanges {
		if c.Address == address {
			return c, true
		}
	}
	return ResourceChange{}, false
}

func (p *Plan) config(address string) (ConfigResource, bool) {
	for _, c := range p.Configuration.RootModule.Resources {
		if c.Address == address {
			return c, true
		}
	}
	return ConfigResource{}, false
}

// Rule is one inline ingress or egress block, flattened into the shape the
// assertions ask questions of.
type Rule struct {
	Owner       string // the security group's address
	Direction   string // "ingress" or "egress"
	Index       int
	Protocol    string
	FromPort    float64
	ToPort      float64
	CIDRs       []string
	Description string
	// ScopedToGroup is true when the block names a security group. It comes
	// from after_unknown, not from values: another resource's id is not known
	// at plan time, so planned_values omits the field entirely and a test
	// reading only planned_values cannot tell this rule from one that names no
	// group at all.
	ScopedToGroup bool
}

func (r Rule) String() string {
	return fmt.Sprintf("%s %s[%d]", r.Owner, r.Direction, r.Index)
}

// Unrestricted reports whether a rule opens everything. protocol "-1" is every
// protocol and every port whatever from_port says, which is why this asks about
// the protocol and not only about the ports.
func (r Rule) Unrestricted() bool {
	return r.Protocol == "-1" || (r.FromPort == 0 && r.ToPort == 65535)
}

func (r Rule) OpenToTheWorld() bool {
	for _, c := range r.CIDRs {
		if c == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

// rules flattens one security group's inline blocks.
func (p *Plan) rules(t *testing.T, address string) []Rule {
	t.Helper()
	res, ok := p.resource(address)
	if !ok {
		t.Fatalf("%s is not in the plan", address)
	}
	unknown := map[string][]any{}
	if ch, ok := p.change(address); ok {
		if m, ok := ch.Change.AfterUnknown.(map[string]any); ok {
			for _, dir := range []string{"ingress", "egress"} {
				if blocks, ok := m[dir].([]any); ok {
					unknown[dir] = blocks
				}
			}
		}
	}

	var out []Rule
	for _, dir := range []string{"ingress", "egress"} {
		blocks, _ := res.Values[dir].([]any)
		for i, b := range blocks {
			m, _ := b.(map[string]any)
			r := Rule{Owner: address, Direction: dir, Index: i}
			r.Protocol, _ = m["protocol"].(string)
			r.Description, _ = m["description"].(string)
			r.FromPort, _ = m["from_port"].(float64)
			r.ToPort, _ = m["to_port"].(float64)
			for _, c := range asSlice(m["cidr_blocks"]) {
				if s, ok := c.(string); ok {
					r.CIDRs = append(r.CIDRs, s)
				}
			}
			if i < len(unknown[dir]) {
				if um, ok := unknown[dir][i].(map[string]any); ok {
					// true when the field is unknown, which for security_groups
					// means "it references a group whose id apply will decide".
					if v, ok := um["security_groups"].(bool); ok && v {
						r.ScopedToGroup = true
					}
				}
			}
			out = append(out, r)
		}
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// references returns the resource addresses an expression named, with the
// bare-resource duplicates Terraform emits alongside each attribute reference
// dropped: it records both "aws_security_group.alb.id" and
// "aws_security_group.alb" for one use.
func (c ConfigResource) references(field string) []string {
	m, ok := c.Expressions[field].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range asSlice(m["references"]) {
		s, ok := r.(string)
		if !ok {
			continue
		}
		// Resource references only: an expression that also reads a local or a
		// variable records those here too, and they are not what "which
		// security group does this name" is asking about.
		if !strings.HasPrefix(s, "aws_") && !strings.HasPrefix(s, "data.aws_") {
			continue
		}
		if strings.Count(s, ".") >= 2 {
			s = s[:strings.LastIndex(s, ".")]
		}
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	// Sorted, because Terraform does not emit this list in a stable order.
	// Measured across three consecutive plans of one configuration: the same
	// ingress block's references came back in three different orders. An
	// assertion comparing an unsorted slice would flake.
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// stringList reads a plan variable that is a list of strings.
func (p *Plan) stringList(t *testing.T, name string) []string {
	t.Helper()
	v, ok := p.Variables[name]
	if !ok {
		t.Fatalf("the plan records no variable %q", name)
	}
	var out []string
	for _, e := range asSlice(v.Value) {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("variable %q holds a non-string entry", name)
		}
		out = append(out, s)
	}
	return out
}

// Container is one entry of a task definition's container_definitions. The
// whole reason it is readable at plan time is that local.image is built from
// variables: one unknown field inside the jsonencode makes the entire string
// unknown, and then nothing below can be asserted at all.
type Container struct {
	Name                   string            `json:"name"`
	Image                  string            `json:"image"`
	User                   string            `json:"user"`
	ReadonlyRootFilesystem bool              `json:"readonlyRootFilesystem"`
	Environment            []NameValue       `json:"environment"`
	Secrets                []NameValueFrom   `json:"secrets"`
	MountPoints            []MountPoint      `json:"mountPoints"`
	PortMappings           []PortMapping     `json:"portMappings"`
	HealthCheck            *HealthCheck      `json:"healthCheck"`
	LinuxParameters        *LinuxParameters  `json:"linuxParameters"`
	LogConfiguration       *LogConfiguration `json:"logConfiguration"`
}

type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type NameValueFrom struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
}

type MountPoint struct {
	SourceVolume  string `json:"sourceVolume"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly"`
}

type PortMapping struct {
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type HealthCheck struct {
	Command []string `json:"command"`
}

type LinuxParameters struct {
	Capabilities struct {
		Drop []string `json:"drop"`
	} `json:"capabilities"`
	InitProcessEnabled bool `json:"initProcessEnabled"`
}

type LogConfiguration struct {
	LogDriver string            `json:"logDriver"`
	Options   map[string]string `json:"options"`
}

// TaskDefinition is one aws_ecs_task_definition, with its containers decoded.
type TaskDefinition struct {
	Address string
	Service string
	// TaskRoleARN is the ambient credential no environment assertion could ever
	// see: ECS injects AWS_CONTAINER_CREDENTIALS_RELATIVE_URI at runtime, not
	// here.
	TaskRoleARN any
	// TaskRoleUnknown is the other half, and without it the assertion is
	// vacuous. Measured while running C10b: task_role_arn set from
	// aws_iam_role.execution.arn is absent from planned_values and marked true
	// in after_unknown, so a test reading only the value sees nil — the same
	// nil that "no task role at all" produces — and passes.
	TaskRoleUnknown bool
	Containers      []Container
	Volumes         []string
}

// taskDefinitions decodes every task definition in the plan, failing rather
// than skipping when container_definitions came back unknown — an assertion
// that silently has nothing to read is the defect this package exists to catch.
func (p *Plan) taskDefinitions(t *testing.T) []TaskDefinition {
	t.Helper()
	var out []TaskDefinition
	for _, r := range p.resources("aws_ecs_task_definition") {
		td := TaskDefinition{Address: r.Address, TaskRoleARN: r.Values["task_role_arn"]}
		if ch, ok := p.change(r.Address); ok {
			if m, ok := ch.Change.AfterUnknown.(map[string]any); ok {
				td.TaskRoleUnknown, _ = m["task_role_arn"].(bool)
			}
		}
		if s, ok := r.Values["family"].(string); ok {
			td.Service = s[strings.LastIndex(s, "-")+1:]
		}
		raw, ok := r.Values["container_definitions"].(string)
		if !ok {
			t.Fatalf("%s container_definitions is unknown at plan time; every assertion about what is inside this container would be vacuous. Something in it interpolates an attribute apply decides.", r.Address)
		}
		if err := json.Unmarshal([]byte(raw), &td.Containers); err != nil {
			t.Fatalf("%s container_definitions is not JSON: %v", r.Address, err)
		}
		for _, v := range asSlice(r.Values["volume"]) {
			if m, ok := v.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					td.Volumes = append(td.Volumes, n)
				}
			}
		}
		out = append(out, td)
	}
	if len(out) == 0 {
		t.Fatal("the plan has no task definitions")
	}
	return out
}

func (t TaskDefinition) envNames() []string {
	var out []string
	for _, c := range t.Containers {
		for _, e := range c.Environment {
			out = append(out, e.Name)
		}
	}
	return out
}

func (t TaskDefinition) env(name string) (string, bool) {
	for _, c := range t.Containers {
		for _, e := range c.Environment {
			if e.Name == name {
				return e.Value, true
			}
		}
	}
	return "", false
}

// taskDefinition returns the one task definition for a service, or fails.
func (p *Plan) taskDefinition(t *testing.T, service string) TaskDefinition {
	t.Helper()
	for _, td := range p.taskDefinitions(t) {
		if td.Service == service {
			return td
		}
	}
	t.Fatalf("no task definition for %s", service)
	return TaskDefinition{}
}
