# make graph: analyse a set of Go repositories and link them in one go.
#
# The run targets (graph, inventory, deploy, checks, diff, bundle, org) are
# `codegraph run <step>`, the steps codegraph runs from a configuration file
# (.codegraph.yaml) too: fleet over clean Go clones (<repo>.git or <repo> with a
# go.mod), reusing the previous run's analysis of those unchanged, linked into
# one system view (system.html is the interactive explorer); then, when
# configured, what an environment's Helm chart deploys, the profile's checks,
# and the comparison with the previous run. The clones stay read-only: the run
# uses GOFLAGS=-mod=readonly and fails if any repository's git status changes.
#
# Settings come from codegraph's layers: the built-in defaults, the profile's
# defaults (PROFILE, or CODEGRAPH_PROFILE in the environment: a directory with
# a profile.yaml), the configuration file's, and the variables below when given
# to make, on its command line or in the environment:
#
#   make graph REPOS_DIR=~/src OUT=/tmp/graph REPOS="$HOME/src/orders.git $HOME/src/billing.git"
#
# make config shows what they resolve to (codegraph config), make check checks
# them, make status tells how current the last run is.
#
#   REPOS_DIR        the directory holding the repositories (root)
#   REPOS            the repositories to analyse (names in REPOS_DIR, or paths)
#   FOCUS            a file naming the repositories around which inventory finds
#                    the frontier, which graph then analyses too; empty: every one
#   INVENTORY RINGS  the inventory's directory, and the frontier's rings
#   OUT              the run's directory (the previous one moves to OUT.prev)
#   RULES REPO_RULES DECLARED INCLUDE_MODULES LIBRARIES API FORMATS ALGO GOPRIVATE
#                    the analysis's settings (codegraph run -h describes them)
#   FULL             non-empty: analyse every repository again, reusing nothing
#   DEPLOY_CHART DEPLOY_ENV  the environment's Helm chart (empty: none) and its name
#   CHECKS           empty: run none of the profile's checks
#   BASE             the run make diff compares with (OUT.prev)
#   BUNDLE BUNDLE_FLAGS  the bundle's file, and flags (-no-settings -no-positions)
#   DENYLIST         words no file of this repository may contain (make audit)

# ---- settings -------------------------------------------------------------------
PROFILE ?= $(CODEGRAPH_PROFILE)
ifneq ($(strip $(PROFILE)),)
override PROFILE := $(abspath $(PROFILE))
endif
# The version codegraph reports (codegraph version) and writes into what it
# produces (manifests, inventory, deployment, bundles): git describe's, the
# latest tag (v1.0.1, or v1.0.1-3-gabc1234 past it, -dirty with changes), unless
# set. Release builds (make release): their platforms (os/arch) and directory.
ifeq ($(origin VERSION),undefined)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
endif
LDFLAGS         = -X main.version=$(VERSION)
PLATFORMS       ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DIST            ?= bin
SHA256          ?= $(if $(shell command -v sha256sum 2>/dev/null),sha256sum,shasum -a 256)
SAMPLES_OUT     ?= tmp/samples
COVER_MIN       ?= 70
IMAGE           ?= codegraph
DOCKER          ?= docker
GOMODCACHE      ?= $(shell go env GOMODCACHE 2>/dev/null || echo $(HOME)/go/pkg/mod)
NETRC           ?= $(wildcard $(HOME)/.netrc)
BIN             ?= tmp/bin/codegraph

# ---- targets --------------------------------------------------------------------
CODEGRAPH ?= go run -ldflags "$(LDFLAGS)" ./cmd/codegraph

# given tells whether a variable was given to make (its command line or the
# environment); str, bool and list turn a given variable into codegraph's flag.
given = $(filter command line environment environment override,$(origin $(1)))
str   = $(if $(call given,$(1)),$(2) '$($(1))')
bool  = $(if $(call given,$(1)),$(2)=$(if $(strip $($(1))),true,false))
list  = $(if $(call given,$(1)),$(if $(strip $($(1))),$(foreach v,$($(1)),$(2) '$(v)'),$(2) ''))
RUN_FLAGS = $(if $(PROFILE),-profile '$(PROFILE)') $(call str,REPOS_DIR,-root) $(call list,REPOS,-repos) $(SETTINGS)
SETTINGS = $(if $(call given,FOCUS),-focus '$(or $(FOCUS),none)') $(call str,INVENTORY,-inventory) $(call str,RINGS,-rings) $(call str,OUT,-out) \
	$(call list,RULES,-rules) $(call str,REPO_RULES,-repo-rules) $(call str,DECLARED,-declared) $(call list,INCLUDE_MODULES,-include-modules) \
	$(call bool,LIBRARIES,-libraries) $(call bool,API,-api) $(call str,FORMATS,-formats) $(call str,ALGO,-algo) $(call str,GOPRIVATE,-goprivate) \
	$(call bool,FULL,-full) $(if $(call given,DEPLOY_CHART),-chart '$(or $(DEPLOY_CHART),none)') $(call str,DEPLOY_ENV,-environment) \
	$(if $(and $(call given,CHECKS),$(if $(strip $(CHECKS)),,x)),-no-checks) $(call str,BASE,-base) $(call str,BUNDLE,-bundle) \
	$(call str,DENYLIST,-denylist) $(BUNDLE_FLAGS)
# get prints one resolved setting (codegraph config -get), for the targets
# that use it here: a step's, run by name like the targets run them, never a
# configuration file's default pipeline.
get = $(shell $(CODEGRAPH) config -pipeline none -get $(1) $(RUN_FLAGS))

.PHONY: help org graph check config status inventory deploy checks diff query open samples test cover audit bundle release clean mcp-build mcp-desktop docker-image docker-test docker-graph
.DEFAULT_GOAL := help

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ printf "  make %-13s %s\n", $$1, $$2 }'

graph: ## Analyse the repositories and link them (reusing the analysis of those unchanged; FULL=1 reuses none), then the deployment, the checks and the diff with the last run, when set
	@$(CODEGRAPH) run graph $(RUN_FLAGS)

org: ## The first time, or to refresh the frontier: graph (the focus), inventory (its frontier, from that run), graph again
	@$(CODEGRAPH) run org $(RUN_FLAGS)

check: ## Check the tools, files and repositories a run needs (codegraph check)
	@$(CODEGRAPH) check -pipeline none $(RUN_FLAGS)

config: ## Show every setting a run resolves, and where it comes from (codegraph config)
	@$(CODEGRAPH) config -pipeline none $(RUN_FLAGS)

status: ## Tell how current the last run is, and which repositories moved since (codegraph status)
	@$(CODEGRAPH) status -pipeline none $(RUN_FLAGS)

inventory: ## Read every repository of REPOS_DIR (no build) and the frontier rings (RINGS) around FOCUS
	@$(CODEGRAPH) run inventory $(RUN_FLAGS)

deploy: ## What the chart deploys (helm template, offline, the chart never changed), matched to the last run
	@$(CODEGRAPH) run deploy $(RUN_FLAGS)

checks: ## Run the profile's checks on the last run (such as a comparison with a knowledge base)
	@$(CODEGRAPH) run checks $(RUN_FLAGS)

diff: ## Compare the last run with the one before it (BASE=<run dir> for another; report in OUT/diff.md)
	@$(CODEGRAPH) run diff $(RUN_FLAGS)

query: ## Ask the last run: make query Q='who <topic>' (go run ./cmd/codegraph query -h lists the questions)
	@$(CODEGRAPH) query -run '$(call get,out)' $(Q)

mcp-build: ## Build codegraph for MCP clients that need a binary (tmp/bin/codegraph); rebuild after code changes
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/codegraph

mcp-desktop: mcp-build ## Print the Claude Desktop entry for the codegraph MCP server (absolute paths)
	@echo 'Add to "mcpServers" in ~/Library/Application Support/Claude/claude_desktop_config.json, then restart Claude Desktop:'
	@printf '%s\n' '"codegraph": {' '  "command": "$(abspath $(BIN))",' '  "args": ["mcp", "-run", "$(call get,out)"]' '}'

open: ## Open the system explorer of the last run
	@page='$(call get,out)/_system/system.html'; test -f "$$page" || { echo "no $$page yet: make graph"; exit 1; }; \
	if command -v open >/dev/null; then open "$$page"; else xdg-open "$$page"; fi

samples: ## Analyse the bundled samples and link them
	$(CODEGRAPH) analyze -dir testdata/orders -rules testdata/orders/codegraph-rules.json -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/inventory -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/ledger -rules testdata/rules/inhouse.json -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/billing -rules testdata/rules/inhouse.json -include-modules 'example.com/inhouse/*-sdk-go' -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/gateway -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/tool -entry ./cmd/... -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/mailer -library -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/pipeline -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/queue -entry ./cmd/... -rules testdata/rules/inhouse.json -out $(SAMPLES_OUT)
	$(CODEGRAPH) analyze -dir testdata/payouts -entry ./cmd/... -out $(SAMPLES_OUT)
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
	@denylist='$(call get,denylist)'; test -n "$$denylist" -a -f "$$denylist" || { echo "no denylist (set DENYLIST, or use a profile that has one)"; exit 1; }; \
	mkdir -p tmp && sed -e 's/\#.*//' -e 's/[[:space:]]*$$//' -e '/^$$/d' "$$denylist" > tmp/.denylist
	@files=$$(if git rev-parse --git-dir >/dev/null 2>&1; then git ls-files --cached --others --exclude-standard; \
		else find . -type f -not -path './.git/*' -not -path './tmp/*' -not -path './.idea/*' $(if $(PROFILE),-not -path '$(PROFILE)/*'); fi); \
	test -n "$$files" || { echo "audit: no file to check"; exit 1; }; \
	if grep -n -i -w -F -f tmp/.denylist $$files; then echo "audit: the files above name what the denylist lists"; exit 1; \
	else echo "audit: no file names any of the $$(wc -l < tmp/.denylist | tr -d ' ') words of the denylist"; fi

bundle: ## Pack the last run into one file to share (BUNDLE: OUT.tar.gz, .tar.zst for zstd), with its deployment and inventory; BUNDLE_FLAGS=-no-settings -no-positions leave those out
	@$(CODEGRAPH) run bundle $(RUN_FLAGS)

release: ## Build codegraph for PLATFORMS (macOS and Linux, amd64 and arm64) into DIST (bin/) with SHA256SUMS; VERSION=v1.2.0 names them
	@mkdir -p $(DIST) && rm -f $(DIST)/codegraph-* $(DIST)/SHA256SUMS
	@for p in $(PLATFORMS); do os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/codegraph-$(VERSION)-$$os-$$arch; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $$out ./cmd/codegraph || exit 1; \
		echo "$$out"; done
	@cd $(DIST) && $(SHA256) codegraph-* > SHA256SUMS
	@echo "$(DIST)/SHA256SUMS: upload it with the binaries"

clean: ## Remove the outputs of graph, inventory and samples, and the built binary
	@out='$(call get,out)'; inv='$(call get,inventory)'; test -n "$$out" -a -n "$$inv" || exit 1; \
	echo "rm -rf $$out $$out.prev $$inv $(SAMPLES_OUT) $(BIN)"; rm -rf "$$out" "$$out.prev" "$$inv" $(SAMPLES_OUT) $(BIN)

# ---- docker ---------------------------------------------------------------------
# The same targets in the toolchain image, for any CI. The repositories are
# mounted read-only, the Go module cache is shared with the host (private
# modules already downloaded need no credentials), and a .netrc, when present,
# lets Go and git fetch the others from any git host. The output directory's
# parent is mounted so the previous run can move aside, and the profile
# read-only. A check reading files outside the profile skips itself or fails
# in the image: CHECKS= runs none.
ROOT_DIR = $(call get,root)
OUT_DIR  = $(call get,out)
DOCKER_RUN = $(DOCKER) run --rm \
	-v $(ROOT_DIR):/repos:ro \
	-v $(dir $(OUT_DIR)):/work \
	-v $(GOMODCACHE):/go/pkg/mod \
	-v codegraph-gocache:/root/.cache/go-build \
	$(if $(NETRC),-v $(NETRC):/root/.netrc:ro) \
	$(if $(PROFILE),-v $(PROFILE):/profile:ro) \
	$(if $(GOPROXY),-e GOPROXY=$(GOPROXY))

docker-image: ## Build the toolchain image (Go, pkg-config and libxml2, python3)
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-test: docker-image ## make test in the image
	$(DOCKER) run --rm $(IMAGE) make test

docker-graph: docker-image ## make graph in the image, with the repositories mounted read-only
	@mkdir -p $(dir $(OUT_DIR))
	$(DOCKER_RUN) $(IMAGE) codegraph run graph -config none $(if $(PROFILE),-profile /profile) $(SETTINGS) \
		-root /repos -out /work/$(notdir $(OUT_DIR)) $(if $(call given,REPOS),$(foreach r,$(REPOS),-repos '$(notdir $(r))'))
