// Command openapiviews regenerates the two audience-specific source views
// from the canonical public bundle. Each view intentionally contains external
// JSON Pointer references rather than duplicating schemas or operations.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

type view struct {
	path        string
	title       string
	description string
	paths       []string
}

func main() {
	bundleBytes, err := os.ReadFile("api/openapi/openapi.yaml")
	if err != nil {
		fail(err)
	}
	var bundle struct {
		OpenAPI string               `yaml:"openapi"`
		Info    map[string]any       `yaml:"info"`
		Paths   map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(bundleBytes, &bundle); err != nil {
		fail(fmt.Errorf("parse public OpenAPI bundle: %w", err))
	}
	if bundle.OpenAPI != "3.1.0" {
		fail(fmt.Errorf("unsupported bundle OpenAPI version %q", bundle.OpenAPI))
	}
	version, _ := bundle.Info["version"].(string)
	if version == "" {
		fail(fmt.Errorf("public OpenAPI bundle has no info.version"))
	}

	views := []view{
		{
			path: "api/openapi/control.yaml", title: "InferScale Control API",
			description: "Source view for the tenant-scoped management API. The release bundle is openapi.yaml.",
			paths: []string{
				"/healthz", "/v1/deployments", "/v1/deployments/{id}",
				"/v1/deployments/{id}/benchmarks", "/v1/deployments/{id}/metrics", "/v1/operations/{id}",
			},
		},
		{
			path: "api/openapi/inference.yaml", title: "InferScale Inference API",
			description: "OpenAI-compatible text-only inference subset served through Envoy Gateway.",
			paths:       []string{"/v1/deployments/{deployment_id}/chat/completions"},
		},
	}
	for _, candidate := range views {
		for _, path := range candidate.paths {
			if _, exists := bundle.Paths[path]; !exists {
				fail(fmt.Errorf("%s refers to missing bundled path %s", candidate.path, path))
			}
		}
		if err := os.WriteFile(candidate.path, render(candidate, version), 0o644); err != nil {
			fail(err)
		}
	}
}

func render(candidate view, version string) []byte {
	var result bytes.Buffer
	fmt.Fprintln(&result, "openapi: 3.1.0")
	fmt.Fprintln(&result, "info:")
	fmt.Fprintf(&result, "  title: %s\n", candidate.title)
	fmt.Fprintf(&result, "  version: %s\n", version)
	fmt.Fprintf(&result, "  description: %s\n", candidate.description)
	fmt.Fprintln(&result, "servers:")
	fmt.Fprintln(&result, "  - url: https://api.inferscale.local")
	fmt.Fprintln(&result, "security:")
	fmt.Fprintln(&result, "  - bearerAPIKey: []")
	fmt.Fprintln(&result, "paths:")
	for _, path := range candidate.paths {
		fmt.Fprintf(&result, "  %s:\n", path)
		fmt.Fprintf(&result, "    $ref: ./openapi.yaml#/paths/%s\n", jsonPointerEscape(path))
	}
	fmt.Fprintln(&result, "components:")
	fmt.Fprintln(&result, "  securitySchemes:")
	fmt.Fprintln(&result, "    bearerAPIKey:")
	fmt.Fprintln(&result, "      $ref: ./openapi.yaml#/components/securitySchemes/bearerAPIKey")
	return result.Bytes()
}

func jsonPointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
