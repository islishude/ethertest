# Vendored specifications

Normal builds are offline. The reviewed OpenRPC, OpenAPI, and minimal/Fulu
constant subsets consumed by the current alpha are stored below this directory;
dependency implementations remain pinned by `go.mod` and `go.sum`.

Automated upstream refresh, digest verification, generated full wire contracts,
license mirroring, and official vector material are still release gates. Until
that update path exists, changes to these reviewed subsets and `spec.lock` must
be made together. A release must not contain a `PIN_REQUIRED` entry.
