# ADR 0009: Put admission at Envoy external authorization

## Context

Inference authentication and trusted scheduling metadata must run before llm-d without making a management service proxy long-lived token streams. Rate limiting also needs a shared tenant-wide bucket, while concurrency and queue lifetime are already understood by llm-d.

## Decision

Attach `inferscale-admission` to the platform Gateway with an Envoy Gateway `SecurityPolicy` using the gRPC external-authorization protocol. The service authenticates the bearer API key, resolves the deployment from the UUID path, enforces ownership and the Valkey-backed tenant rate bucket, rejects the unsupported OpenAI request surface, removes client-provided `x-llm-d-*` scheduling headers, and adds trusted fairness/priority metadata. Envoy removes the credential before forwarding an accepted inference request. Valkey or PostgreSQL errors fail new requests closed with `503`; existing streams never depend on admission after acceptance.

Concurrency, queue bounds, fairness ordering, cancellation, and stream lifetime stay in the revision EPP. The API is not an inference proxy and Valkey is not a request queue.

## Alternatives considered

- API middleware on the data path: rejected because it would proxy every stream.
- A custom reverse proxy/activator: rejected as duplicate data-plane infrastructure.
- Per-pod in-memory rate limits: rejected because replicas would not enforce a tenant-wide budget.

## Consequences

Gateway external-auth body buffering, route recomputation, header mutation, and streaming preservation are release conformance requirements. An ext-auth outage intentionally stops new inference requests.
