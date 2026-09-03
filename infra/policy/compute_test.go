//go:build tfplan

package policy

import (
	"slices"
	"strings"
	"testing"
)

// admit.DefaultHosts. Written out rather than imported, so that widening the Go
// default is not the same edit as widening the deployment's — the assertion is
// a subset check against what this project ships, and a deliberately
// github-only deployment stays legal.
var defaultHosts = []string{"github.com", "codeberg.org"}

// §2's sentence as a resource: "splitting it makes the sandbox a deployment
// boundary rather than a code convention" is false the moment the two processes
// share one set of firewall rules.
//
// Asserted by rule content, because a count survives an edit that points the
// indexer's service at the gateway's group. What CANNOT be asserted here is the
// link from a service to its group: network_configuration.security_groups holds
// an id apply decides, and aws_ecs_service.svc is a for_each whose one
// configuration entry references local.services without a per-key breakdown. So
// this asserts that two differently-shaped groups exist and that the difference
// is the one clause 1's table claims; that each service is wired to its own is
// the gap, and it is recorded rather than papered over.
func TestTheIndexerAndTheGatewayHaveDifferentSecurityGroups(t *testing.T) {
	p := load(t)
	for _, svc := range []string{"gateway", "indexer"} {
		if _, ok := p.resource("aws_security_group." + svc); !ok {
			t.Fatalf("aws_security_group.%s is not in the plan; §2's split is back to a directory layout", svc)
		}
	}

	gw := p.config2refs(t, "aws_security_group.gateway", "ingress")
	ix := p.config2refs(t, "aws_security_group.indexer", "ingress")
	if slices.Equal(gw, ix) {
		t.Fatalf("both groups admit the same source %v; the two are then one boundary wearing two names", gw)
	}
	// The gateway is reachable from the load balancer; the indexer is not
	// reachable from it at all.
	if !slices.Equal(gw, []string{"aws_security_group.alb"}) {
		t.Errorf("the gateway's ingress references %v, want only the load balancer", gw)
	}
	if slices.Contains(ix, "aws_security_group.alb") {
		t.Errorf("the indexer's ingress references the load balancer's group")
	}
}

// config2refs is the resource references one field of a resource's
// configuration made.
func (p *Plan) config2refs(t *testing.T, address, field string) []string {
	t.Helper()
	c, ok := p.config(address)
	if !ok {
		t.Fatalf("no configuration for %s", address)
	}
	return c.references(field)
}

func TestTheIndexerHasNoUnrestrictedEgress(t *testing.T) {
	p := load(t)
	for _, r := range p.rules(t, "aws_security_group.indexer") {
		if r.Direction != "egress" {
			continue
		}
		// A single -1 rule is what every ECS example ships and it opens tcp/22,
		// tcp/9418 — the git:// scheme spec §4 refuses at admission — and every
		// UDP port.
		if r.Unrestricted() {
			t.Errorf("%s protocol %q ports %v-%v is unrestricted", r, r.Protocol, r.FromPort, r.ToPort)
		}
	}
}

// The test the plan named TestTheIndexersEgressPortsAreExactly443And53, which
// its own decision text contradicts: the worker also has to reach Postgres.
// 443, 5432 and 53 is the whole legitimate set.
func TestTheIndexersEgressPortsAreExactly443And5432AndDNS(t *testing.T) {
	p := load(t)
	got := map[float64]bool{}
	for _, r := range p.rules(t, "aws_security_group.indexer") {
		if r.Direction != "egress" {
			continue
		}
		if r.FromPort != r.ToPort {
			t.Errorf("%s spans %v-%v; egress here is enumerated, never a range", r, r.FromPort, r.ToPort)
		}
		got[r.FromPort] = true
	}
	// 11434 is the embedder. Both binaries make one real round trip to it at
	// boot and fatal if it fails, so an egress set without it is a stack that
	// cannot start — which the plan file's enumerated list left out.
	for _, want := range []float64{443, 5432, 53, 11434} {
		if !got[want] {
			t.Errorf("the indexer has no egress on %v", want)
		}
		delete(got, want)
	}
	for extra := range got {
		t.Errorf("the indexer has egress on %v, which nothing in §4 needs", extra)
	}
}

func TestTheIndexerHasNoIngressExceptTheScrapePort(t *testing.T) {
	p := load(t)
	var ingress []Rule
	for _, r := range p.rules(t, "aws_security_group.indexer") {
		if r.Direction == "ingress" {
			ingress = append(ingress, r)
		}
	}
	if len(ingress) != 1 {
		t.Fatalf("the indexer has %d ingress rules, want exactly the scrape port", len(ingress))
	}
	r := ingress[0]
	if r.FromPort != 9090 || r.ToPort != 9090 || r.Protocol != "tcp" {
		t.Errorf("%s admits %s/%v-%v, want tcp/9090", r, r.Protocol, r.FromPort, r.ToPort)
	}
	if len(r.CIDRs) != 0 {
		t.Errorf("%s admits CIDRs %v; the untrusted-input component accepts a scrape from a security group and nothing else", r, r.CIDRs)
	}
	// From after_unknown, because a security group id is not known at plan
	// time and planned_values omits the field entirely — so a rule with no
	// group at all would look identical here without this.
	if !r.ScopedToGroup {
		t.Errorf("%s names no security group", r)
	}
	c, ok := p.config("aws_security_group.indexer")
	if !ok {
		t.Fatal("no configuration for aws_security_group.indexer")
	}
	if refs := c.references("ingress"); !slices.Equal(refs, []string{"aws_security_group.observability"}) {
		t.Errorf("the indexer's ingress references %v, want only the scraper's group", refs)
	}
}

// The failure this catches is silent: adding a target group costs nothing at
// apply time and publishes spec §2's untrusted-input component to the internet.
func TestNoTargetGroupPointsAtTheIndexer(t *testing.T) {
	p := load(t)
	for _, tg := range p.resources("aws_lb_target_group") {
		if strings.Contains(tg.Address, "indexer") {
			t.Errorf("%s exists", tg.Address)
		}
		if n, _ := tg.Values["name"].(string); strings.Contains(n, "indexer") {
			t.Errorf("%s is named %q", tg.Address, n)
		}
	}
	for _, svc := range p.resources("aws_ecs_service") {
		if !strings.Contains(svc.Address, "indexer") {
			continue
		}
		if lb := asSlice(svc.Values["load_balancer"]); len(lb) != 0 {
			t.Errorf("%s has %d load_balancer blocks, want none", svc.Address, len(lb))
		}
	}
	for _, rule := range p.resources("aws_lb_listener_rule") {
		c, ok := p.config(rule.Address)
		if !ok {
			continue
		}
		for _, ref := range c.references("action") {
			if strings.Contains(ref, "indexer") {
				t.Errorf("%s routes to %s", rule.Address, ref)
			}
		}
	}
}

// health.go answers /health with an unconditional 200, so a load balancer
// probing it keeps a gateway in rotation that has lost Postgres and is failing
// every query behind a healthy target. Asserted together with the container
// check below, because asserting one is how the other goes wrong.
func TestTheLoadBalancerHealthCheckPathIsReady(t *testing.T) {
	p := load(t)
	tg, ok := p.resource("aws_lb_target_group.gateway")
	if !ok {
		t.Fatal("aws_lb_target_group.gateway is not in the plan")
	}
	checks := asSlice(tg.Values["health_check"])
	if len(checks) != 1 {
		t.Fatalf("aws_lb_target_group.gateway has %d health checks", len(checks))
	}
	m, _ := checks[0].(map[string]any)
	if path, _ := m["path"].(string); path != "/ready" {
		t.Errorf("aws_lb_target_group.gateway health_check.path is %q, want \"/ready\"", path)
	}
}

// The mirror: a container check on readiness turns one shared-datastore outage
// into a rolling restart of every task, and the process that could still serve
// /metrics and say why is the one being killed.
func TestTheContainerHealthCheckProbesLiveness(t *testing.T) {
	p := load(t)
	var checked int
	for _, td := range p.taskDefinitions(t) {
		for _, c := range td.Containers {
			if c.HealthCheck == nil {
				continue
			}
			checked++
			cmd := strings.Join(c.HealthCheck.Command, " ")
			if !strings.Contains(cmd, "-probe") {
				t.Errorf("%s health check is %q; the image has no shell and only the binary can make the request", td.Address, cmd)
			}
			if strings.Contains(cmd, "/ready") {
				t.Errorf("%s container health check probes readiness", td.Address)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no container has a health check; this assertion would be vacuous")
	}
}

// The whole key set, in P4's style: an assertion that one dangerous name is
// absent passes when a different dangerous name is added.
func TestTheIndexersEnvironmentIsExactlyTheAllowlist(t *testing.T) {
	p := load(t)
	td := p.taskDefinition(t, "indexer")

	want := []string{
		"ALLOWED_HOSTS", "EMBED_MODEL", "EMBED_PROVIDER", "HOME",
		"JOB_DEADLINE_SECONDS", "KEEP_REPOS", "MAX_REPO_BYTES", "MAX_REPO_FILES",
		"OLLAMA_URL", "PROBE_PORT", "SCRATCH_DIR", "TYPECHECK",
	}
	got := td.envNames()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the indexer's environment is %v, want exactly %v", got, want)
	}

	// TYPECHECK_GOPROXY is closed by omission, so graph.go's default of `off`
	// applies. If it ever appears it must be `off`: anything else turns a
	// stranger's require line into an outbound fetch to a host of their
	// choosing.
	if v, ok := td.env("TYPECHECK_GOPROXY"); ok && v != "off" {
		t.Errorf("TYPECHECK_GOPROXY is %q; anything but off is the SSRF §4 refuses, reached by a road the host allowlist cannot see", v)
	}
}

func TestAllowedHostsIsSetExplicitlyAndIsASubsetOfTheDefault(t *testing.T) {
	p := load(t)
	for _, svc := range []string{"gateway", "indexer"} {
		td := p.taskDefinition(t, svc)
		v, ok := td.env("ALLOWED_HOSTS")
		if !ok {
			t.Errorf("%s does not set ALLOWED_HOSTS; the allowlist would then be whatever the binary defaults to, unreviewed", svc)
			continue
		}
		for _, h := range strings.Split(v, ",") {
			// A subset and not an equality, so widening fails and a
			// deliberately github-only deployment stays legal.
			if !slices.Contains(defaultHosts, strings.TrimSpace(h)) {
				t.Errorf("%s ALLOWED_HOSTS contains %q, which is not in admit.DefaultHosts", svc, h)
			}
		}
	}
}

// The hole the environment allowlist cannot see, because the variable is not in
// the artifact being asserted over: with a task role, ECS injects
// AWS_CONTAINER_CREDENTIALS_RELATIVE_URI at runtime and 169.254.170.2 becomes a
// working credential endpoint — link-local, so no security-group rule touches
// it — inside the untrusted-input process. clone.Run appends to os.Environ(),
// so it reaches git too.
func TestNoTaskDefinitionHasATaskRole(t *testing.T) {
	p := load(t)
	for _, td := range p.taskDefinitions(t) {
		if td.TaskRoleARN != nil {
			t.Errorf("%s sets task_role_arn to %v; nothing in this codebase calls an AWS API, so the role would grant something and buy nothing", td.Address, td.TaskRoleARN)
		}
		// The half that actually catches it. A role assigned from another
		// resource's arn is unknown at plan time, so the value above is nil —
		// exactly as it is when there is no role at all.
		if td.TaskRoleUnknown {
			t.Errorf("%s assigns a task_role_arn that apply decides; 169.254.170.2 becomes a live credential endpoint inside the untrusted-input process, over link-local where no security-group rule applies", td.Address)
		}
	}
}

func TestNoContainerRunsAsRootOrWithAWritableRootFilesystem(t *testing.T) {
	p := load(t)
	for _, td := range p.taskDefinitions(t) {
		for _, c := range td.Containers {
			if c.User != "65532:65532" {
				t.Errorf("%s container %s runs as %q, want 65532:65532", td.Address, c.Name, c.User)
			}
			if !c.ReadonlyRootFilesystem {
				t.Errorf("%s container %s has a writable root filesystem", td.Address, c.Name)
			}
			if c.LinuxParameters == nil {
				t.Errorf("%s container %s has no linuxParameters", td.Address, c.Name)
				continue
			}
			if !slices.Equal(c.LinuxParameters.Capabilities.Drop, []string{"ALL"}) {
				t.Errorf("%s container %s drops %v, want [ALL]", td.Address, c.Name, c.LinuxParameters.Capabilities.Drop)
			}
			// The indexer forks git and go; with the application as pid 1 an
			// unreaped child is a zombie per job.
			if !c.LinuxParameters.InitProcessEnabled {
				t.Errorf("%s container %s has no init process", td.Address, c.Name)
			}
			for _, m := range c.MountPoints {
				if m.ReadOnly {
					continue
				}
				// The embedder's model directory is its own; the console's two
				// are nginx's cache and pidfile. Every entry here is a path
				// somebody had to write down.
				if !slices.Contains([]string{"/scratch", "/tmp", "/.ollama", "/var/cache/nginx", "/var/run"}, m.ContainerPath) {
					t.Errorf("%s container %s can write %s", td.Address, c.Name, m.ContainerPath)
				}
			}
		}
	}
}

// Without it a bad image is a crash-looping service and a green pipeline.
func TestEveryServiceHasACircuitBreakerThatRollsBack(t *testing.T) {
	p := load(t)
	services := p.resources("aws_ecs_service")
	if len(services) == 0 {
		t.Fatal("the plan has no ECS services")
	}
	for _, s := range services {
		cb := asSlice(s.Values["deployment_circuit_breaker"])
		if len(cb) != 1 {
			t.Errorf("%s has %d circuit breakers, want one", s.Address, len(cb))
			continue
		}
		m, _ := cb[0].(map[string]any)
		if enable, _ := m["enable"].(bool); !enable {
			t.Errorf("%s circuit breaker is not enabled", s.Address)
		}
		if rollback, _ := m["rollback"].(bool); !rollback {
			t.Errorf("%s circuit breaker does not roll back", s.Address)
		}
	}
}

// P5's console has no CORS to fall back on and no configurable API base — its
// API base is the relative prefix /api — so a second origin is a console that
// cannot talk to anything.
func TestTheApiAndTheConsoleShareOneOrigin(t *testing.T) {
	p := load(t)
	if n := len(p.resources("aws_lb")); n != 1 {
		t.Fatalf("the stack has %d load balancers, want exactly one origin", n)
	}
	rules := p.resources("aws_lb_listener_rule")
	if len(rules) != 1 {
		t.Fatalf("the listener has %d rules, want one for /api/*", len(rules))
	}
	if got := listenerPaths(rules[0]); !slices.Equal(got, []string{"/api/*"}) {
		t.Errorf("the listener rule matches %v, want [/api/*]", got)
	}
}

// The probes and the metrics have no listener rule and are reachable only by
// the target group health check and, for /metrics, by the scraper inside the
// VPC. The default action is a fixed 404 rather than the gateway for exactly
// this reason: health.go mounts all three at the root.
func TestNoListenerRulePublishesTheProbeOrMetricsPaths(t *testing.T) {
	p := load(t)
	forbidden := []string{"/health", "/ready", "/metrics"}
	for _, r := range p.resources("aws_lb_listener_rule") {
		for _, path := range listenerPaths(r) {
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) || path == "/*" || path == "/" {
					t.Errorf("%s publishes %q", r.Address, path)
				}
			}
		}
	}
	for _, l := range p.resources("aws_lb_listener") {
		for _, a := range asSlice(l.Values["default_action"]) {
			m, _ := a.(map[string]any)
			kind, _ := m["type"].(string)
			if kind == "forward" {
				// Only legal when a console is the default; otherwise every
				// path the gateway serves at the root would be published.
				c, _ := p.config(l.Address)
				refs := c.references("default_action")
				if !slices.Contains(refs, "aws_lb_target_group.console") {
					t.Errorf("%s forwards by default to %v; with the gateway as the default action, /health, /ready and /metrics are all published", l.Address, refs)
				}
			}
		}
	}
}

func listenerPaths(r Resource) []string {
	var out []string
	for _, c := range asSlice(r.Values["condition"]) {
		m, _ := c.(map[string]any)
		for _, pp := range asSlice(m["path_pattern"]) {
			pm, _ := pp.(map[string]any)
			for _, v := range asSlice(pm["values"]) {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}
