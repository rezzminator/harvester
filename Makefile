# The checks GitHub runs on a pull request to main (.github/workflows/release.yml),
# runnable here first: `make release-check` runs them all.
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)
# go.mod's toolchain exactly, as setup-go gives CI; build-release.sh refuses any other.
export GOTOOLCHAIN := go$(GO_VERSION)
# CI=true on runners, where setup-node gives Node 22.
NODE := $(if $(CI),,mise exec node@22 --)

# Semver order: a release sorts after its own pre-releases (0.1.0 > 0.1.0-dev.2), which sort -V gets wrong.
define VERSION_PAST
import sys
def key(v):
    core, _, pre = v.split("+")[0].partition("-")
    ids = [(0, int(i), "") if i.isdigit() else (1, 0, i) for i in pre.split(".")] if pre else []
    return [int(x) for x in core.split(".")], not pre, ids
base, version = sys.argv[1:]
if key(version) <= key(base):
    sys.exit(f"release: version {version} does not move past main's {base}")
print(f"release: version {version} moves past main's {base}")
endef
export VERSION_PAST

.PHONY: release-check check leak release

release-check: check leak release

check:
	unformatted=$$(sh -c '"$$(go env GOROOT)/bin/gofmt" -l .'); \
	test -z "$$unformatted" || { printf '%s\n' "$$unformatted"; exit 1; }
	# umask 022 as on the GitHub runner: the daemon refuses group-writable test binaries.
	umask 022 && go build ./... && go vet ./... && go test ./...
	umask 022 && CGO_ENABLED=1 go test -race ./internal/harvest ./internal/harvestmcp
	$(NODE) node --test plugins/harvester/opencode.test.mjs
	scripts/release-check.sh

# LEAK_TERMS names the terms file (CI writes the secret to one); unset, scripts/leak-terms.txt.
leak: SHELL := bash
leak:
	mapfile -d '' -t sources < <(git ls-files -z); \
	scripts/leak-check.sh --files "$${sources[@]}"

# The version must move past main's VERSION, then the committed sums must match a fresh build.
release:
	git rev-parse --verify --quiet origin/main >/dev/null || { echo "release: origin/main not found; fetch it first" >&2; exit 1; }
	base=$$(git show origin/main:VERSION 2>/dev/null); \
	python3 -c "$$VERSION_PAST" "$${base:-0.0.0}" "$$(cat VERSION)"
	scripts/release-check.sh --release
