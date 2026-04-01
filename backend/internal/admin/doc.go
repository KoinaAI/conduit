// Package admin exposes the RESTful administrative surface used to manage
// providers, routes, pricing profiles, integrations, gateway keys, and
// operational inspection data for a personal Conduit deployment.
//
// The public README intentionally avoids duplicating a full endpoint reference.
// Detailed admin API documentation is expected to be generated from:
//   - the handler comments in this package
//   - the route registration in internal/app
//   - the OpenAPI document served by /api/admin/openapi.json
package admin
