//go:build tfplan

package policy

import (
	"regexp"
	"strings"
	"testing"
)

// The way this goes wrong is not a variable called PASSWORD. It is DATABASE_URL
// with the password already interpolated into it, sitting in `environment`
// because that was the shortest path from a working local setup — so this scans
// values as well as names.
func TestNoSecretValueAppearsInAContainersPlaintextEnvironment(t *testing.T) {
	p := load(t)
	byName := regexp.MustCompile(`(?i)(_PASSWORD|_KEY|_TOKEN|_SECRET|_URI|^DATABASE_URL$|^PASSWORD$)`)
	byValue := regexp.MustCompile(`(?i)(postgres(ql)?://|://[^/@\s]+:[^/@\s]+@)`)
	highEntropy := regexp.MustCompile(`^[A-Za-z0-9+/_-]{32,}$`)

	for _, td := range p.taskDefinitions(t) {
		for _, c := range td.Containers {
			for _, e := range c.Environment {
				// The offending KEY, never the matched value: an assertion that
				// prints the secret it found has moved the leak rather than
				// closed it.
				if byName.MatchString(e.Name) {
					t.Errorf("%s: environment key %s looks like a secret", td.Address, e.Name)
				}
				if byValue.MatchString(e.Value) {
					t.Errorf("%s: environment key %s holds a credential-bearing URI", td.Address, e.Name)
				}
				if highEntropy.MatchString(e.Value) {
					t.Errorf("%s: environment key %s holds a long opaque string", td.Address, e.Name)
				}
			}
		}
	}
}

func TestTheDatabaseURLIsInjectedAsASecretAndNeverAsEnvironment(t *testing.T) {
	p := load(t)
	for _, svc := range []string{"gateway", "indexer"} {
		td := p.taskDefinition(t, svc)
		if _, ok := td.env("DATABASE_URL"); ok {
			t.Errorf("%s has DATABASE_URL in environment, which anyone with ecs:DescribeTaskDefinition can read", svc)
		}
		var found bool
		for _, c := range td.Containers {
			for _, name := range c.Secrets {
				if name == "DATABASE_URL" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s does not receive DATABASE_URL through the secrets block", svc)
		}
	}
}

// verify-full and not require: `require` encrypts and does not authenticate the
// server, so it stops a passive listener and not a man in the middle, and
// nothing in any log distinguishes the two.
//
// Asserted through the variables rather than through the rendered DSN, because
// that string interpolates the database endpoint and is unknown at plan time.
// The second half — that the DSN actually uses them — is the reference check
// below, without which this would assert two variables nothing reads.
func TestTheDSNVerifiesTheServerCertificate(t *testing.T) {
	p := load(t)

	mode := p.stringVar(t, "db_sslmode")
	if mode != "verify-full" {
		t.Errorf("DATABASE_URL sslmode is %q, want \"verify-full\"", mode)
	}
	if mode == "disable" || mode == "require" {
		t.Errorf("sslmode %q does not authenticate the server", mode)
	}

	cert := p.stringVar(t, "db_ssl_root_cert")
	if cert == "" {
		t.Error("DATABASE_URL has sslmode=verify-full and no sslrootcert; every certificate in the RDS bundle is a self-signed root and the image's default store cannot validate the server")
	}
	if !strings.HasSuffix(cert, ".pem") {
		t.Errorf("db_ssl_root_cert is %q, which is not a certificate bundle path", cert)
	}

	// And that the DSN is built from both, so the two variables are not just
	// values nothing reads. local.database_url is what the secret version
	// interpolates, and the configuration records what its expression named.
	c, ok := p.config("aws_secretsmanager_secret_version.database_url")
	if !ok {
		t.Fatal("no configuration for aws_secretsmanager_secret_version.database_url")
	}
	refs := c.varReferences("secret_string")
	for _, want := range []string{"var.db_sslmode", "var.db_ssl_root_cert", "var.db_password"} {
		if !contains(refs, want) {
			t.Errorf("the DSN expression does not reference %s; it references %v", want, refs)
		}
	}
}

func TestTheDatabaseIsNotPubliclyAccessible(t *testing.T) {
	p := load(t)
	dbs := p.resources("aws_db_instance")
	if len(dbs) != 1 {
		t.Fatalf("the plan has %d database instances, want one", len(dbs))
	}
	db := dbs[0]
	if v, _ := db.Values["publicly_accessible"].(bool); v {
		t.Error("aws_db_instance.main is publicly accessible")
	}
	if v, _ := db.Values["storage_encrypted"].(bool); !v {
		t.Error("aws_db_instance.main storage is not encrypted")
	}
	// Private subnets, which have no route to the internet gateway.
	sg, ok := p.config("aws_db_subnet_group.main")
	if !ok {
		t.Fatal("no configuration for aws_db_subnet_group.main")
	}
	if refs := sg.references("subnet_ids"); !contains(refs, "aws_subnet.private") {
		t.Errorf("the database's subnet group references %v, want the private subnets", refs)
	}
}

func TestOnlyTheApplicationSecurityGroupsCanReachPort5432(t *testing.T) {
	p := load(t)
	var ingress []Rule
	for _, r := range p.rules(t, "aws_security_group.data") {
		if r.Direction == "ingress" {
			ingress = append(ingress, r)
		}
	}
	if len(ingress) != 2 {
		t.Fatalf("the data security group has %d ingress rules, want two", len(ingress))
	}
	for _, r := range ingress {
		if r.FromPort != 5432 || r.ToPort != 5432 {
			t.Errorf("%s admits %v-%v, want 5432", r, r.FromPort, r.ToPort)
		}
		if len(r.CIDRs) != 0 {
			t.Errorf("%s admits CIDRs %v; never a CIDR, never the load balancer", r, r.CIDRs)
		}
		if !r.ScopedToGroup {
			t.Errorf("%s names no security group", r)
		}
	}
	c, ok := p.config("aws_security_group.data")
	if !ok {
		t.Fatal("no configuration for aws_security_group.data")
	}
	refs := c.references("ingress")
	for _, want := range []string{"aws_security_group.gateway", "aws_security_group.indexer"} {
		if !contains(refs, want) {
			t.Errorf("the database's ingress does not reference %s; it references %v", want, refs)
		}
	}
	for _, got := range refs {
		if got != "aws_security_group.gateway" && got != "aws_security_group.indexer" {
			t.Errorf("the database admits %s", got)
		}
	}
}

func TestTheEngineVersionIsPostgres17(t *testing.T) {
	p := load(t)
	for _, db := range p.resources("aws_db_instance") {
		if e, _ := db.Values["engine"].(string); e != "postgres" {
			t.Errorf("%s engine is %q", db.Address, e)
		}
		v, _ := db.Values["engine_version"].(string)
		if !strings.HasPrefix(v, "17.") {
			t.Errorf("%s engine_version is %q, want a 17 minor", db.Address, v)
		}
	}
}

func TestTheEmbedProviderIsNeverFake(t *testing.T) {
	p := load(t)
	if got := p.stringVar(t, "embed_provider"); got != "ollama" {
		t.Errorf("embed_provider is %q; a fake embedder serves meaningless retrieval and every response is still a 200", got)
	}
	for _, svc := range []string{"gateway", "indexer"} {
		td := p.taskDefinition(t, svc)
		if v, ok := td.env("EMBED_PROVIDER"); !ok || v != "ollama" {
			t.Errorf("%s EMBED_PROVIDER is %q (set=%v)", svc, v, ok)
		}
	}
}

// At plan time, naming the secret — rather than at apply with AWS's
// "SecretString must not be empty", or at runtime with a crash-looping task.
func TestEverySecretHasANonEmptyValuePrecondition(t *testing.T) {
	p := load(t)
	versions := p.resources("aws_secretsmanager_secret_version")
	secrets := p.resources("aws_secretsmanager_secret")
	if len(versions) == 0 || len(versions) != len(secrets) {
		t.Fatalf("%d secrets and %d versions; every secret must have exactly one", len(secrets), len(versions))
	}
	guarded := preconditionSources(t)
	for _, v := range versions {
		addr := v.Address
		if i := strings.Index(addr, "["); i >= 0 {
			addr = addr[:i]
		}
		if !guarded[addr] {
			t.Errorf("%s has no lifecycle precondition; an empty value would fail at apply with AWS's own message, or crash-loop a task", v.Address)
		}
	}
}

// The sibling project's registry-credentials bug was a secret created outside
// the map the IAM grant iterated.
func TestTheExecutionRoleGrantIteratesTheSecretMap(t *testing.T) {
	p := load(t)
	c, ok := p.config("data.aws_iam_policy_document.execution")
	if !ok {
		t.Fatal("no configuration for the execution policy document")
	}
	if refs := c.allReferences("statement"); !contains(refs, "aws_secretsmanager_secret.app") {
		t.Errorf("the execution role's grant references %v, not the secret map itself", refs)
	}
}
