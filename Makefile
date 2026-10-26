# make graph: analyse a set of Go repositories and link them in one go.
#
# Runs `codegraph fleet` over clean Go clones (<repo>.git with a go.mod), links
# them into one system view (system.html is the interactive explorer), and, when
# configured, reads what an environment's Helm chart deploys, compares the run
# with a knowledge base, and with the previous run. The clones stay read-only:
# the run uses GOFLAGS=-mod=readonly and fails if any repository's git status
# changes.
#
# An organisation's settings live in a profile (PROFILE, or CODEGRAPH_PROFILE in
# the environment): a directory whose profile.mk sets the variables below, read
# before their defaults here. Without one, every Go clone of REPOS_DIR is
# analysed with the built-in rules only.
#
# Every setting can be overridden on the command line or in the environment, e.g.
#
#   make graph REPOS_DIR=~/src OUT=/tmp/graph REPOS="$HOME/src/orders.git $HOME/src/billing.git"

# ---- profile --------------------------------------------------------------------
PROFILE ?= $(CODEGRAPH_PROFILE)
ifneq ($(strip $(PROFILE)),)
override PROFILE := $(abspath $(PROFILE))
include $(PROFILE)/profile.mk
endif

# ---- config ---------------------------------------------------------------------
REPOS_DIR       ?= ..
# A file naming the repositories to analyse (one per line, # comments), around
# which `make inventory` finds the frontier; empty analyses every Go clone.
FOCUS           ?=
INVENTORY       ?= tmp/inventory
RINGS           ?= 1
DEPLOY_ENV      ?= production
# The environment's Helm chart, rendered offline by `make deploy` (needs helm).
DEPLOY_CHART    ?=
NAMES           = $(shell sed -e 's/\#.*//' $(FOCUS) $(wildcard $(INVENTORY)/frontier.txt) 2>/dev/null)
REPOS           ?= $(patsubst %/go.mod,%,$(wildcard $(if $(FOCUS),$(addprefix $(REPOS_DIR)/,$(addsuffix .git/go.mod,$(NAMES))),$(REPOS_DIR)/*.git/go.mod)))
OUT             ?= tmp/graph
# Rules for in-house libraries, and a directory of per-repository rule files.
RULES           ?=
REPO_RULES      ?=
# Services codegraph cannot analyse (other languages), described by hand.
DECLARED        ?=
# Client libraries analysed with the services that use them (space-separated
# module patterns, e.g. example.com/*-sdk-go).
INCLUDE_MODULES ?=
# Non-empty: analyse the repositories without a main as libraries (their
# exported API, never linked) instead of skipping them.
LIBRARIES       ?=
# Non-empty: also analyse the API of the repositories with mains, as <name>-api
# (implies LIBRARIES).
API             ?=
FORMATS         ?= json,md,html,agents
ALGO            ?= vta
GOPRIVATE       ?=
# A knowledge base check: a command given the run directory, printing JSON.
KBCHECK         ?=
# Words no file of this repository may contain (make audit), one per line.
DENYLIST        ?=
SAMPLES_OUT     ?= tmp/samples
COVER_MIN       ?= 70
# Release builds (make release): the version they report, the platforms
# (os/arch) and where they go.
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
PLATFORMS       ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DIST            ?= bin
SHA256          ?= $(if $(shell command -v sha256sum 2>/dev/null),sha256sum,shasum -a 256)
IMAGE           ?= codegraph
DOCKER          ?= docker
GOMODCACHE      ?= $(shell go env GOMODCACHE 2>/dev/null || echo $(HOME)/go/pkg/mod)
NETRC           ?= $(wildcard $(HOME)/.netrc)
BIN             ?= tmp/bin/codegraph
BASE            ?= $(OUT).prev

# ---- targets --------------------------------------------------------------------
CODEGRAPH ?= go run ./cmd/codegraph
SYSTEM = $(OUT)/_system

# status records every repository's git status, to show the run changed none.
status = for r in $(REPOS); do echo "== $$r"; git -C "$$r" status --porcelain 2>/dev/null || echo "(not a git repository)"; done > $(1)

.PHONY: help org graph check fleet inventory deploy kbcheck diff query open samples test cover audit release clean mcp-build mcp-desktop docker-image docker-test docker-graph
.DEFAULT_GOAL := help

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ printf "  make %-13s %s\n", $$1, $$2 }'

graph: fleet ## Analyse every repository and link them; then what DEPLOY_CHART deploys, the knowledge base check and the last run, when set
	@if [ -z "$(DEPLOY_CHART)" ]; then echo "deployment skipped (no DEPLOY_CHART)"; \
	elif command -v helm >/dev/null && [ -d "$(DEPLOY_CHART)" ]; then $(MAKE) --no-print-directory deploy; \
	else echo "deployment skipped (needs helm and $(DEPLOY_CHART))"; fi
	@$(MAKE) --no-print-directory kbcheck
	@if ls $(OUT).prev/*/manifest.json >/dev/null 2>&1; then $(MAKE) --no-print-directory diff BASE=$(OUT).prev; fi
	@echo "explorer: $(SYSTEM)/system.html (make open)"

org: ## The first time, or to refresh the frontier: graph (the focus), inventory (its frontier, from that run), graph again
	@$(MAKE) --no-print-directory graph
	@$(MAKE) --no-print-directory inventory
	@$(MAKE) --no-print-directory graph

check: ## Check the tools and repositories the run needs
	@command -v go >/dev/null || { echo "go is not installed"; exit 1; }
	@test -n "$(strip $(REPOS))" || { echo "no Go repositories in $(REPOS_DIR)/*.git (set REPOS_DIR or REPOS)"; exit 1; }
	@command -v pkg-config >/dev/null || echo "warning: pkg-config is missing, so cgo repositories needing C libraries will fail; on macOS: brew install pkgconf"
	@echo "$(words $(REPOS)) repositories from $(REPOS_DIR)$(if $(FOCUS), ($(FOCUS)$(if $(wildcard $(INVENTORY)/frontier.txt), and $(INVENTORY)/frontier.txt))) -> $(OUT)"

fleet: check ## Analyse every repository and link them (codegraph fleet); the last run moves to $(OUT).prev
	@if [ -d $(SYSTEM) ]; then rm -rf $(OUT).prev && mv $(OUT) $(OUT).prev; fi
	@mkdir -p $(OUT)
	@$(call status,$(OUT)/.git-status-before)
	$(if $(GOPRIVATE),GOPRIVATE=$(GOPRIVATE)) GOFLAGS=-mod=readonly $(CODEGRAPH) fleet -algo $(ALGO) \
		$(if $(RULES),-rules $(RULES)) $(if $(REPO_RULES),-repo-rules $(REPO_RULES)) \
		$(if $(and $(DECLARED),$(wildcard $(DECLARED)/*.json)),-declared $(DECLARED)) \
		$(foreach m,$(INCLUDE_MODULES),-include-modules '$(m)') $(if $(LIBRARIES),-libraries) $(if $(API),-api) -format $(FORMATS) -out $(OUT) $(REPOS)
	@$(call status,$(OUT)/.git-status-after)
	@diff -u $(OUT)/.git-status-before $(OUT)/.git-status-after || { echo "a repository's git status changed during the run (diff above). codegraph never writes to them: if someone edited a clone meanwhile, run again"; exit 1; }

inventory: ## Read every repository of REPOS_DIR (no build) and the frontier rings (RINGS) around FOCUS
	@test -f $(SYSTEM)/system.json || echo "no run in $(OUT): the frontier is found from the inventory alone (make graph, then make inventory again, is more precise)"
	$(CODEGRAPH) inventory -root $(REPOS_DIR) -out $(INVENTORY) $(if $(PROFILE),-profile $(PROFILE)) $(if $(FOCUS),-focus $(FOCUS) -rings $(RINGS)) $(if $(wildcard $(SYSTEM)/system.json),-run $(OUT))
	@echo "report: $(INVENTORY)/inventory.md$(if $(FOCUS), and $(INVENTORY)/frontier.md; make graph now analyses the frontier too)"

deploy: ## What DEPLOY_ENV (production) runs: helm template, offline, the chart never changed; matched to the last run
	@command -v helm >/dev/null || { echo "helm is not installed (on macOS: brew install helm)"; exit 1; }
	@test -n "$(DEPLOY_CHART)" -a -d "$(DEPLOY_CHART)" || { echo "no chart in '$(DEPLOY_CHART)' (set DEPLOY_CHART)"; exit 1; }
	@git -C "$(DEPLOY_CHART)" status --porcelain > $(OUT).chart-before 2>/dev/null || true
	$(CODEGRAPH) deploy -chart $(DEPLOY_CHART) -env $(DEPLOY_ENV) -out $(OUT)/deploy \
		$(if $(wildcard $(INVENTORY)/inventory.json),-inventory $(INVENTORY)/inventory.json) $(if $(wildcard $(OUT)/*/manifest.json),-run $(OUT))
	@git -C "$(DEPLOY_CHART)" status --porcelain > $(OUT).chart-after 2>/dev/null || true
	@diff -u $(OUT).chart-before $(OUT).chart-after && rm -f $(OUT).chart-before $(OUT).chart-after || { echo "the chart's git status changed during the run (diff above)"; exit 1; }
	@if ls $(OUT)/*/manifest.json >/dev/null 2>&1; then $(CODEGRAPH) link -format html -deploy $(OUT)/deploy/deploy.json -out $(SYSTEM) $(OUT)/*/manifest.json; fi
	@echo "report: $(OUT)/deploy/deploy.md (and $(DEPLOY_ENV) in $(SYSTEM)/system.html)"

kbcheck: ## Compare the last run with a knowledge base (KBCHECK, when set)
	@test -f $(SYSTEM)/system.json || { echo "no run in $(OUT) yet: make graph"; exit 1; }
	@if [ -z "$(strip $(KBCHECK))" ]; then echo "no knowledge base check (KBCHECK); skipped"; \
	else $(KBCHECK) $(OUT) > $(OUT)/kbcheck.json && echo "details: $(OUT)/kbcheck.json"; fi

diff: ## Compare a run with the last one: make diff [BASE=<run dir>] (report in $(OUT)/diff.md)
	@test -d "$(BASE)" || { echo "no run to compare with in $(BASE) (set BASE)"; exit 1; }
	@$(CODEGRAPH) diff -base $(BASE) -head $(OUT) -out $(OUT)/diff.md && echo "changes: $(OUT)/diff.md"

query: ## Ask the last run: make query Q='who <topic>' (go run ./cmd/codegraph query -h lists the questions)
	@$(CODEGRAPH) query -run $(OUT) $(Q)

mcp-build: ## Build codegraph for MCP clients that need a binary (tmp/bin/codegraph); rebuild after code changes
	go build -o $(BIN) ./cmd/codegraph

mcp-desktop: mcp-build ## Print the Claude Desktop entry for the codegraph MCP server (absolute paths)
	@echo 'Add to "mcpServers" in ~/Library/Application Support/Claude/claude_desktop_config.json, then restart Claude Desktop:'
	@printf '%s\n' '"codegraph": {' '  "command": "$(abspath $(BIN))",' '  "args": ["mcp", "-run", "$(abspath $(OUT))"]' '}'

open: ## Open the system explorer of the last run
	@test -f $(SYSTEM)/system.html || { echo "no $(SYSTEM)/system.html yet: make graph"; exit 1; }
	@if command -v open >/dev/null; then open $(SYSTEM)/system.html; else xdg-open $(SYSTEM)/system.html; fi

samples: ## Analyse the bundled samples and link them
	$(CODEGRAPH) analyze -dir testdata/orders -rules testdata/orders/codegraph-rules.json -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/inventory -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/ledger -rules testdata/rules/inhouse.json -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/billing -rules testdata/rules/inhouse.json -include-modules 'example.com/inhouse/*-sdk-go' -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/gateway -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/tool -entry ./cmd/... -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/mailer -library -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/pipeline -out $(SAMPLES_OUT)
	$(CODEGRAPH) link -out $(SAMPLES_OUT)/_system $(SAMPLES_OUT)/*/manifest.json

test: ## gofmt, go vet and the tests (with a profile, its rule files are checked too)
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	go vet ./...
	$(if $(PROFILE),CODEGRAPH_PROFILE=$(PROFILE)) go test ./...

cover: ## Statement coverage of the whole test suite (the golden tests exercise every package); fails under COVER_MIN %
	@mkdir -p tmp
	@go test ./... -coverpkg=./... -coverprofile=tmp/cover.out > tmp/cover.log 2>&1 || { tail -20 tmp/cover.log; exit 1; }
	@go tool cover -func=tmp/cover.out | awk '/^total:/ { gsub("%", "", $$3); printf "coverage: %s%% of statements (minimum $(COVER_MIN)%%); details: go tool cover -html=tmp/cover.out\n", $$3; if ($$3 + 0 < $(COVER_MIN)) exit 1 }'

audit: ## Check that no file of this repository contains a word of DENYLIST (a profile's private names)
	@test -n "$(DENYLIST)" -a -f "$(DENYLIST)" || { echo "no denylist (set DENYLIST, or use a profile that has one)"; exit 1; }
	@mkdir -p tmp && sed -e 's/\#.*//' -e 's/[[:space:]]*$$//' -e '/^$$/d' $(DENYLIST) > tmp/.denylist
	@files=$$(if git rev-parse --git-dir >/dev/null 2>&1; then git ls-files --cached --others --exclude-standard; \
		else find . -type f -not -path './.git/*' -not -path './tmp/*' -not -path './.idea/*' $(if $(PROFILE),-not -path '$(PROFILE)/*'); fi); \
	test -n "$$files" || { echo "audit: no file to check"; exit 1; }; \
	if grep -n -i -w -F -f tmp/.denylist $$files; then echo "audit: the files above name what $(DENYLIST) lists"; exit 1; \
	else echo "audit: no file names any of the $$(wc -l < tmp/.denylist | tr -d ' ') words of $(DENYLIST)"; fi

release: ## Build codegraph for PLATFORMS (macOS and Linux, amd64 and arm64) into DIST (bin/) with SHA256SUMS; VERSION=v1.2.0 names them
	@mkdir -p $(DIST) && rm -f $(DIST)/codegraph-* $(DIST)/SHA256SUMS
	@for p in $(PLATFORMS); do os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/codegraph-$(VERSION)-$$os-$$arch; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/codegraph || exit 1; \
		echo "$$out"; done
	@cd $(DIST) && $(SHA256) codegraph-* > SHA256SUMS
	@echo "$(DIST)/SHA256SUMS: upload it with the binaries"

clean: ## Remove the outputs of graph, inventory and samples, and the built binary
	rm -rf $(OUT) $(OUT).prev $(INVENTORY) $(SAMPLES_OUT) $(BIN)

# ---- docker ---------------------------------------------------------------------
# The same targets in the toolchain image, for any CI. The repositories are
# mounted read-only, the Go module cache is shared with the host (private
# modules already downloaded need no credentials), and a .netrc, when present,
# lets Go and git fetch the others from any git host. The output directory's
# parent is mounted so the previous run can move aside.
DOCKER_RUN = $(DOCKER) run --rm \
	-v $(abspath $(REPOS_DIR)):/repos:ro \
	-v $(abspath $(dir $(OUT))):/work \
	-v $(GOMODCACHE):/go/pkg/mod \
	-v codegraph-gocache:/root/.cache/go-build \
	$(if $(KB),-v $(KB):/kb/architecture.md:ro) \
	$(if $(NETRC),-v $(NETRC):/root/.netrc:ro) \
	$(if $(PROFILE),-v $(PROFILE):/profile:ro) \
	$(if $(GOPRIVATE),-e GOPRIVATE=$(GOPRIVATE)) $(if $(GOPROXY),-e GOPROXY=$(GOPROXY))

docker-image: ## Build the toolchain image (Go, pkg-config and libxml2, python3)
	$(DOCKER) build -t $(IMAGE) .

docker-test: docker-image ## make test in the image
	$(DOCKER) run --rm $(IMAGE) make test

docker-graph: docker-image ## make graph in the image, with the repositories mounted read-only
	@mkdir -p $(dir $(OUT))
	$(DOCKER_RUN) $(IMAGE) make graph $(if $(PROFILE),PROFILE=/profile) REPOS_DIR=/repos OUT=/work/$(notdir $(OUT)) \
		REPOS="$(patsubst $(REPOS_DIR)/%,/repos/%,$(REPOS))" $(if $(KB),KB=/kb/architecture.md,KB=)
