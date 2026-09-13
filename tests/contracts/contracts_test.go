package contracts_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	yaml "go.yaml.in/yaml/v3"
	rbacv1 "k8s.io/api/rbac/v1"
	kyaml "sigs.k8s.io/yaml"
)

var repositoryRoot = mustRepositoryRoot()

func TestOpenAPIBundleAndSourceViews(t *testing.T) {
	bundle := readYAMLDocument(t, filepath.Join(repositoryRoot, "api/openapi/openapi.yaml"))
	control := readYAMLDocument(t, filepath.Join(repositoryRoot, "api/openapi/control.yaml"))
	inference := readYAMLDocument(t, filepath.Join(repositoryRoot, "api/openapi/inference.yaml"))

	if got := scalarAt(t, bundle, "openapi"); got != "3.1.0" {
		t.Fatalf("bundled OpenAPI version=%q", got)
	}
	wantControlPaths := []string{
		"/healthz", "/v1/deployments", "/v1/deployments/{id}",
		"/v1/deployments/{id}/benchmarks", "/v1/deployments/{id}/metrics", "/v1/operations/{id}",
	}
	assertExactMappingKeys(t, mappingAt(t, control, "paths"), wantControlPaths)
	assertExactMappingKeys(t, mappingAt(t, inference, "paths"), []string{"/v1/deployments/{deployment_id}/chat/completions"})
	if mapValue(mappingAt(t, bundle, "paths"), "/readyz") != nil {
		t.Fatal("public OpenAPI bundle must not advertise cluster-internal dependency readiness")
	}
	for _, source := range []*yaml.Node{control, inference} {
		sourcePaths := mappingAt(t, source, "paths")
		for index := 0; index < len(sourcePaths.Content); index += 2 {
			path := sourcePaths.Content[index].Value
			ref := scalarAt(t, sourcePaths.Content[index+1], "$ref")
			wantRef := "./openapi.yaml#/paths/" + jsonPointerEscape(path)
			if ref != wantRef {
				t.Errorf("source path %q references %q, want %q", path, ref, wantRef)
			}
		}
		if got := scalarAt(t, source, "components", "securitySchemes", "bearerAPIKey", "$ref"); got != "./openapi.yaml#/components/securitySchemes/bearerAPIKey" {
			t.Errorf("source bearerAPIKey ref=%q", got)
		}
	}

	documents := map[string]*yaml.Node{
		filepath.Join(repositoryRoot, "api/openapi/openapi.yaml"):   bundle,
		filepath.Join(repositoryRoot, "api/openapi/control.yaml"):   control,
		filepath.Join(repositoryRoot, "api/openapi/inference.yaml"): inference,
	}
	for path, document := range documents {
		path, document := path, document
		t.Run(filepath.Base(path)+"_references", func(t *testing.T) {
			walkNodes(document, func(node *yaml.Node) {
				if node.Kind != yaml.MappingNode {
					return
				}
				ref := mapValue(node, "$ref")
				if ref == nil {
					return
				}
				if _, err := resolveReference(path, scalar(ref), documents); err != nil {
					t.Errorf("unresolved $ref %q: %v", scalar(ref), err)
				}
			})
		})
	}

	operations := map[string]string{}
	paths := mappingAt(t, bundle, "paths")
	for index := 0; index < len(paths.Content); index += 2 {
		path := paths.Content[index].Value
		pathItem := paths.Content[index+1]
		for methodIndex := 0; methodIndex < len(pathItem.Content); methodIndex += 2 {
			method := pathItem.Content[methodIndex].Value
			if !isHTTPMethod(method) {
				continue
			}
			operationID := mapValue(pathItem.Content[methodIndex+1], "operationId")
			if operationID == nil || scalar(operationID) == "" {
				t.Errorf("%s %s has no operationId", strings.ToUpper(method), path)
				continue
			}
			id := scalar(operationID)
			if previous, exists := operations[id]; exists {
				t.Errorf("operationId %q reused by %s and %s %s", id, previous, strings.ToUpper(method), path)
			}
			operations[id] = strings.ToUpper(method) + " " + path
		}
	}

	problem := mappingAt(t, bundle, "components", "schemas", "Problem")
	assertSequenceContains(t, sequenceAt(t, problem, "required"), "code", "request_id")
	if mapValue(mappingAt(t, bundle, "components", "responses", "NotFound", "content"), "application/problem+json") == nil {
		t.Fatal("NotFound is not application/problem+json")
	}

	model := mappingAt(t, bundle, "components", "schemas", "ModelSpec", "properties")
	if got := scalarAt(t, model, "uri", "pattern"); got != `^hf://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$` {
		t.Fatalf("ModelSpec.uri pattern=%q; it must match the CRD's exact org/model grammar", got)
	}
	if got := scalarAt(t, model, "revision", "pattern"); got != `^[0-9a-f]{40}$` {
		t.Fatalf("ModelSpec.revision pattern=%q; lowercase is the cache-key canonical form", got)
	}

	benchmarkOperation := mappingAt(t, bundle, "paths", "/v1/deployments/{id}/benchmarks", "post")
	if got := scalarAt(t, benchmarkOperation, "x-inferscale-scope"); got != "benchmarks:run" {
		t.Fatalf("benchmark creation scope=%q", got)
	}
	if mapValue(mappingAt(t, benchmarkOperation, "responses"), "409") == nil {
		t.Fatal("benchmark creation does not document the not-ready/idempotency conflict response")
	}
	benchmarkList := mappingAt(t, bundle, "paths", "/v1/deployments/{id}/benchmarks", "get")
	if !nodeContainsScalar(benchmarkList, "#/components/parameters/Cursor") {
		t.Fatal("benchmark list does not expose opaque cursor pagination")
	}
	if nodeContainsScalar(benchmarkList, "offset") {
		t.Fatal("benchmark list exposes offset pagination")
	}

	inferenceOperation := mappingAt(t, bundle, "paths", "/v1/deployments/{deployment_id}/chat/completions", "post")
	if !nodeContainsScalar(inferenceOperation, "deployment_id") {
		t.Fatal("inference operation does not bind the public deployment_id path parameter")
	}
	chatRequest := mappingAt(t, bundle, "components", "schemas", "ChatCompletionRequest")
	if got := scalarAt(t, chatRequest, "additionalProperties"); got != "false" {
		t.Fatalf("ChatCompletionRequest.additionalProperties=%q", got)
	}
	message := mappingAt(t, chatRequest, "properties", "messages", "items")
	if got := scalarAt(t, message, "additionalProperties"); got != "false" {
		t.Fatalf("chat message additionalProperties=%q", got)
	}
}

func TestCRDGenerationAndValidationContract(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join(repositoryRoot, "api/crds/platform.inferscale.io_inferencedeployments.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deployed, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy/base/inferscale/crd/platform.inferscale.io_inferencedeployments.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, deployed) {
		t.Fatal("deployed CRD differs from the generated canonical CRD; run make generate")
	}
	text := string(canonical)
	for _, required := range []string{
		"subresources:\n      status: {}",
		"self.accelerator.count == self.runtime.tensorParallelism",
		"self.scaling.minReplicas <= self.scaling.maxReplicas",
		"pattern: ^[0-9a-f]{40}$",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("generated CRD is missing %q", required)
		}
	}
	if strings.Contains(text, "[0-9a-fA-F]{40}") {
		t.Fatal("generated CRD accepts non-canonical uppercase model revisions")
	}
}

func TestEveryCheckedInYAMLDocumentHasUniqueMappingKeys(t *testing.T) {
	var paths []string
	err := filepath.WalkDir(repositoryRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			base := entry.Name()
			if base == ".git" || base == ".mypy_cache" || base == ".pytest_cache" || base == ".ruff_cache" || base == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if extension := filepath.Ext(path); extension == ".yaml" || extension == ".yml" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		path := path
		t.Run(strings.TrimPrefix(path, repositoryRoot+string(filepath.Separator)), func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			decoder := yaml.NewDecoder(file)
			for documentIndex := 0; ; documentIndex++ {
				var document yaml.Node
				err := decoder.Decode(&document)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("decode document %d: %v", documentIndex+1, err)
				}
				assertUniqueKeys(t, &document, fmt.Sprintf("document %d", documentIndex+1))
			}
		})
	}
}

func TestPinnedIntegrationMatrixDeclaresAllReleaseGates(t *testing.T) {
	lock := readYAMLDocument(t, filepath.Join(repositoryRoot, "versions.lock.yaml"))
	if scalarAt(t, lock, "policy", "requireChecksummedDependencyManifests") != "true" ||
		scalarAt(t, lock, "policy", "requireDigestPinnedDependencyWorkloads") != "true" {
		t.Fatal("versions.lock.yaml must fail closed on unchecked manifests or mutable dependency workloads")
	}
	if scalarAt(t, lock, "conformance", "requiredSuite") != "tests/conformance/gateway" {
		t.Fatal("versions.lock.yaml must point at the checked-in Gateway conformance harness")
	}
	features := sequenceAt(t, mappingAt(t, lock, "conformance"), "requiredFeatures")
	assertSequenceContains(t, features,
		"external-authorization",
		"full-duplex-ext-proc",
		"inference-pool-backends",
		"backend-request-header-modifier",
		"weighted-backends",
		"fractional-request-mirror",
		"url-rewrite",
		"trace-continuity",
	)
	digestPattern := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	checksumPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	httpsPattern := regexp.MustCompile(`^https://[^[:space:]@]+/[^[:space:]]+$`)
	components := mappingAt(t, lock, "components")
	for _, required := range []string{
		"envoyGateway", "gatewayAPI", "gatewayAPIInferenceExtension", "llmd", "keda", "vllm", "tensorrtLLM", "prometheusOperator",
	} {
		component := mapValue(components, required)
		if component == nil || scalarAt(t, component, "version") == "" {
			t.Errorf("versions lock omits required conformance component %s", required)
		}
	}
	for index := 0; index < len(components.Content); index += 2 {
		name := components.Content[index].Value
		component := components.Content[index+1]
		if component.Kind != yaml.MappingNode {
			continue
		}
		digest := mapValue(component, "imageDigest")
		if digest != nil && !digestPattern.MatchString(scalar(digest)) {
			t.Errorf("components.%s.imageDigest is not an immutable sha256: %q", name, scalar(digest))
		}
	}
	llmd := mappingAt(t, components, "llmd")
	if !digestPattern.MatchString(scalarAt(t, llmd, "endpointPickerDigest")) {
		t.Error("llm-d endpoint picker is not pinned by digest")
	}

	manifestLocks := map[string][][2]string{
		"gatewayAPI":                   {{"manifestURL", "manifestSHA256"}},
		"envoyGateway":                 {{"manifestURL", "manifestSHA256"}},
		"gatewayAPIInferenceExtension": {{"manifestURL", "manifestSHA256"}},
		"llmd":                         {{"objectiveCRDURL", "objectiveCRDSHA256"}},
		"keda":                         {{"manifestURL", "manifestSHA256"}},
		"prometheusOperator": {
			{"serviceMonitorCRDURL", "serviceMonitorCRDSHA256"},
			{"podMonitorCRDURL", "podMonitorCRDSHA256"},
		},
	}
	for componentName, locks := range manifestLocks {
		component := mappingAt(t, components, componentName)
		for _, fields := range locks {
			url := scalarAt(t, component, fields[0])
			checksum := scalarAt(t, component, fields[1])
			if !httpsPattern.MatchString(url) {
				t.Errorf("components.%s.%s is not a credential-free HTTPS URL: %q", componentName, fields[0], url)
			}
			if !checksumPattern.MatchString(checksum) {
				t.Errorf("components.%s.%s is not a lowercase SHA-256 checksum: %q", componentName, fields[1], checksum)
			}
		}
	}
	for _, imageLock := range []struct {
		component  string
		repository string
		digest     string
	}{
		{component: "envoyGateway", repository: "image", digest: "imageDigest"},
		{component: "envoyGateway", repository: "dataPlaneImage", digest: "dataPlaneImageDigest"},
		{component: "envoyGateway", repository: "rateLimitImage", digest: "rateLimitImageDigest"},
		{component: "keda", repository: "image", digest: "imageDigest"},
		{component: "keda", repository: "metricsServerImage", digest: "metricsServerImageDigest"},
	} {
		component := mappingAt(t, components, imageLock.component)
		repository := scalarAt(t, component, imageLock.repository)
		digest := scalarAt(t, component, imageLock.digest)
		if strings.ContainsAny(repository, "@ \t\r\n") {
			t.Errorf("components.%s.%s is not an image repository: %q", imageLock.component, imageLock.repository, repository)
		}
		if !digestPattern.MatchString(digest) {
			t.Errorf("components.%s.%s is not an immutable digest: %q", imageLock.component, imageLock.digest, digest)
		}
	}
}

func TestDeployedControllerRBACIsGeneratedAndLeastPrivilege(t *testing.T) {
	generatedPath := filepath.Join(repositoryRoot, "deploy/base/inferscale/generated/role.yaml")
	content, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := kyaml.Unmarshal(content, &role); err != nil {
		t.Fatal(err)
	}
	if role.Name != "inferscale-controller" {
		t.Fatalf("generated role name=%q", role.Name)
	}
	want := map[string][]string{
		"|configmaps":                          {"get", "create", "patch", "delete"},
		"|endpoints":                           {"get", "list", "watch"},
		"|pods":                                {"get", "list", "watch"},
		"|serviceaccounts":                     {"get", "create", "patch", "delete"},
		"|services":                            {"get", "create", "patch", "delete"},
		"apps|deployments":                     {"get", "create", "patch", "delete"},
		"batch|jobs":                           {"get", "create", "patch", "delete"},
		"coordination.k8s.io|leases":           {"get", "create", "patch", "delete"},
		"discovery.k8s.io|endpointslices":      {"get", "list", "watch"},
		"gateway.networking.k8s.io|httproutes": {"get", "create", "patch", "delete"},
		"inference.networking.k8s.io|inferencepools":  {"get", "create", "patch", "delete"},
		"keda.sh|scaledobjects":                       {"get", "create", "patch", "delete"},
		"llm-d.ai|inferenceobjectives":                {"get", "create", "patch", "delete"},
		"llm-d.ai|inferencemodelrewrites":             {"get", "list", "watch"},
		"monitoring.coreos.com|servicemonitors":       {"get", "create", "patch", "delete"},
		"networking.k8s.io|networkpolicies":           {"get", "create", "patch", "delete"},
		"platform.inferscale.io|inferencedeployments": {"get", "list", "watch", "update", "patch"},
		"rbac.authorization.k8s.io|roles":             {"get", "create", "patch", "delete"},
		"rbac.authorization.k8s.io|rolebindings":      {"get", "create", "patch", "delete"},
	}
	grants := flattenRBAC(role.Rules)
	for resource, verbs := range grants {
		if strings.Contains(resource, "*") || verbs["*"] || verbs["escalate"] || verbs["bind"] {
			t.Fatalf("controller delegation must use explicit resource permissions, got %s: %v", resource, verbs)
		}
	}
	for resource, verbs := range want {
		for _, verb := range verbs {
			if !grants[resource][verb] {
				t.Errorf("generated controller role lacks %s on %s", verb, resource)
			}
		}
	}
	for _, forbidden := range []string{
		"|secrets", "|namespaces", "|persistentvolumeclaims", "apps|statefulsets", "apps|replicasets",
		"gateway.networking.k8s.io|referencegrants", "inference.networking.k8s.io|inferencemodels",
		"monitoring.coreos.com|podmonitors",
	} {
		if len(grants[forbidden]) > 0 {
			t.Errorf("generated controller role unexpectedly grants %s", forbidden)
		}
	}
	if grants["platform.inferscale.io|inferencedeployments"]["create"] || grants["platform.inferscale.io|inferencedeployments"]["delete"] {
		t.Fatal("controller can create/delete API-owned InferenceDeployments")
	}
	for _, resource := range []string{"|endpoints", "discovery.k8s.io|endpointslices", "llm-d.ai|inferencemodelrewrites"} {
		for verb := range grants[resource] {
			if verb != "get" && verb != "list" && verb != "watch" {
				t.Errorf("controller must only read %s, unexpectedly grants %s", resource, verb)
			}
		}
	}

	// Kubernetes forbids creating a Role that grants permissions its creator
	// lacks. Check the actual EPP Role so future renderer changes cannot bypass
	// the deployed controller's least-privilege contract.
	rendered, err := (routing.Renderer{Config: routing.Config{EndpointPickerImage: "epp@sha256:deadbeef"}}).
		RenderRevision(platformruntime.Spec{
			DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", RoutingPolicy: "load-aware",
		}, platformruntime.Revision{Name: "chat-rev"}, "chat-rev-runtime")
	if err != nil {
		t.Fatal(err)
	}
	delegatedRoles := 0
	for _, object := range rendered.Objects {
		delegated, ok := object.(*rbacv1.Role)
		if !ok {
			continue
		}
		delegatedRoles++
		for resource, verbs := range flattenRBAC(delegated.Rules) {
			for verb := range verbs {
				if !grants[resource][verb] {
					t.Errorf("controller cannot delegate EPP Role %s: lacks %s on %s", delegated.Name, verb, resource)
				}
			}
		}
	}
	if delegatedRoles == 0 {
		t.Fatal("no EPP Role rendered for delegation check")
	}

	kustomization, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy/base/inferscale/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kustomization), "- generated/role.yaml") {
		t.Fatal("generated controller role is not deployed by Kustomize")
	}
	handwritten, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy/base/inferscale/rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(handwritten, []byte("kind: ClusterRole\nmetadata:\n  name: inferscale-controller")) {
		t.Fatal("handwritten controller ClusterRole shadows the generated least-privilege role")
	}
}

func flattenRBAC(rules []rbacv1.PolicyRule) map[string]map[string]bool {
	result := map[string]map[string]bool{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "|" + resource
				if result[key] == nil {
					result[key] = map[string]bool{}
				}
				for _, verb := range rule.Verbs {
					result[key][verb] = true
				}
			}
		}
	}
	return result
}

func readYAMLDocument(t *testing.T, path string) *yaml.Node {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("%s: expected one YAML document", path)
	}
	return document.Content[0]
}

func mappingAt(t *testing.T, node *yaml.Node, path ...string) *yaml.Node {
	t.Helper()
	current := node
	for _, segment := range path {
		if current.Kind != yaml.MappingNode {
			t.Fatalf("%s traverses a non-mapping YAML node", strings.Join(path, "."))
		}
		current = mapValue(current, segment)
		if current == nil {
			t.Fatalf("missing YAML path %s", strings.Join(path, "."))
		}
	}
	if current.Kind != yaml.MappingNode {
		t.Fatalf("YAML path %s is not a mapping", strings.Join(path, "."))
	}
	return current
}

func scalarAt(t *testing.T, node *yaml.Node, path ...string) string {
	t.Helper()
	current := node
	for _, segment := range path {
		if current.Kind != yaml.MappingNode {
			t.Fatalf("%s traverses a non-mapping YAML node", strings.Join(path, "."))
		}
		current = mapValue(current, segment)
		if current == nil {
			t.Fatalf("missing YAML path %s", strings.Join(path, "."))
		}
	}
	return scalar(current)
}

func sequenceAt(t *testing.T, node *yaml.Node, path ...string) *yaml.Node {
	t.Helper()
	current := node
	for _, segment := range path {
		current = mapValue(current, segment)
		if current == nil {
			t.Fatalf("missing YAML path %s", strings.Join(path, "."))
		}
	}
	if current.Kind != yaml.SequenceNode {
		t.Fatalf("YAML path %s is not a sequence", strings.Join(path, "."))
	}
	return current
}

func mapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func walkNodes(node *yaml.Node, visit func(*yaml.Node)) {
	visit(node)
	for _, child := range node.Content {
		walkNodes(child, visit)
	}
}

func resolveReference(currentPath, reference string, documents map[string]*yaml.Node) (*yaml.Node, error) {
	parts := strings.SplitN(reference, "#", 2)
	targetPath := currentPath
	if parts[0] != "" {
		targetPath = filepath.Clean(filepath.Join(filepath.Dir(currentPath), parts[0]))
	}
	target := documents[targetPath]
	if target == nil {
		return nil, fmt.Errorf("document %s was not loaded", targetPath)
	}
	if len(parts) == 1 || parts[1] == "" {
		return target, nil
	}
	if !strings.HasPrefix(parts[1], "/") {
		return nil, fmt.Errorf("unsupported non-pointer fragment %q", parts[1])
	}
	current := target
	for _, encoded := range strings.Split(strings.TrimPrefix(parts[1], "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		switch current.Kind {
		case yaml.MappingNode:
			current = mapValue(current, segment)
		case yaml.SequenceNode:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(current.Content) {
				return nil, fmt.Errorf("invalid sequence index %q", segment)
			}
			current = current.Content[index]
		default:
			current = nil
		}
		if current == nil {
			return nil, fmt.Errorf("pointer segment %q does not exist", segment)
		}
	}
	return current, nil
}

func assertExactMappingKeys(t *testing.T, mapping *yaml.Node, want []string) {
	t.Helper()
	got := make([]string, 0, len(mapping.Content)/2)
	for index := 0; index < len(mapping.Content); index += 2 {
		got = append(got, mapping.Content[index].Value)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mapping keys=%v, want %v", got, want)
	}
}

func assertSequenceContains(t *testing.T, sequence *yaml.Node, want ...string) {
	t.Helper()
	values := map[string]bool{}
	for _, item := range sequence.Content {
		values[item.Value] = true
	}
	for _, value := range want {
		if !values[value] {
			t.Errorf("sequence does not contain %q", value)
		}
	}
}

func nodeContainsScalar(node *yaml.Node, want string) bool {
	found := false
	walkNodes(node, func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && node.Value == want {
			found = true
		}
	})
	return found
}

func assertUniqueKeys(t *testing.T, node *yaml.Node, location string) {
	t.Helper()
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index].Value
			if seen[key] {
				t.Errorf("%s contains duplicate mapping key %q on line %d", location, key, node.Content[index].Line)
			}
			seen[key] = true
		}
	}
	for _, child := range node.Content {
		assertUniqueKeys(t, child, location)
	}
}

func isHTTPMethod(value string) bool {
	switch value {
	case "get", "put", "post", "delete", "patch", "head", "options", "trace":
		return true
	default:
		return false
	}
}

func jsonPointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func mustRepositoryRoot() string {
	workingDirectory, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	current := workingDirectory
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			panic("repository root not found")
		}
		current = parent
	}
}
