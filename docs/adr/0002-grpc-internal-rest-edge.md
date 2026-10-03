# ADR-0002: gRPC internally, REST at the edge

**Status:** Accepted

## Context
Services need typed, efficient contracts between them. External clients (browsers, curl, third parties) expect HTTP/JSON.

## Decision
Use gRPC with Protocol Buffers for service-to-service calls. Expose REST/JSON only through api-gateway.

## Alternatives considered
REST everywhere: no enforced contract, no generated clients, no built-in deadlines. GraphQL: unneeded complexity here. grpc-gateway: generates the REST layer but hides logic; a small hand-written gateway on the standard library's net/http router is easier to read and test, and adds no dependency.

## Consequences
Two API styles to maintain; the gateway stays thin to limit that. Gains: schema-first contracts, generated code, deadline and cancellation propagation, `buf breaking` checks in CI.
