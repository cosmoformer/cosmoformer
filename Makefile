SHELL := /bin/sh
GO := $(if $(wildcard .build/tools/go/bin/go),$(CURDIR)/.build/tools/go/bin/go,go)
TEST_ARGS ?=
export GOCACHE := $(CURDIR)/.build/go-cache
export GOPATH := $(CURDIR)/.build/gopath
export PATH := $(firstword $(wildcard $(CURDIR)/.build/tools/node-v26.13.0-*/bin)):$(PATH)
export COSMOFORMER_TEST_OWNER_DSN ?= $(shell cat .build/testdb/owner.dsn 2>/dev/null)
export COSMOFORMER_TEST_MARKER_FILE ?= $(CURDIR)/.build/testdb/marker

.PHONY: brand tools deps dev-setup dev-api dev-web dev-stop contract test-db-start test-db-stop test-unit test-integration test-source test-native test-scripts worker-image test-coverage test-go-coverage test-linux-coverage test-contract-coverage test-web-unit test-web-coverage test-web browser-server check

brand:
	node scripts/brand.mjs

tools:
	node scripts/tools.mjs

deps:
	$(GO) mod download
	cd web && npm ci --ignore-scripts

contract:
	cd web && COSMOFORMER_GO=$(GO) npm run contract

dev-setup:
	node scripts/dev.mjs setup

dev-api:
	COSMOFORMER_LISTEN=127.0.0.1:18080 COSMOFORMER_ORIGIN=https://localhost:5173 COSMOFORMER_DATABASE_DSN_FILE=$(CURDIR)/.build/dev/app.dsn $(GO) run ./cmd/cosmoformer serve

dev-web: contract
	cd web && npm run dev

dev-stop:
	node scripts/dev.mjs stop

test-db-start:
	node scripts/dev.mjs test-start

test-db-stop:
	node scripts/dev.mjs test-stop

test-unit:
	$(GO) test -race ./internal/... ./cmd/...
	$(GO) vet ./...

test-integration:
	$(GO) test -race -tags=integration ./internal/store ./internal/coordinator ./internal/httpapi ./cmd/... ./tests/acceptance $(TEST_ARGS)

# Local Linux verification: only loopback fixture servers remain reachable at run
# time. No database, cloud credential, developer home or Docker socket is mounted.
test-source:
	docker build --quiet --platform linux/amd64 --target source-tests -f deploy/worker/Dockerfile -t cosmoformer-worker-source-tests:local .
	docker run --rm --network none --platform linux/amd64 cosmoformer-worker-source-tests:local

# Closed, public fixture manifest only. Validate every record before any build;
# runtime containers receive no host environment, network, mounts or credentials.
# Export the literal program instead of passing quoted recipe continuations.
# GNU Make 3.81 and 4.x must pass the same JavaScript argument to Node.
define NATIVE_TEST_SCRIPT
import fs from "node:fs"; import {spawnSync} from "node:child_process";
const m=JSON.parse(fs.readFileSync("deploy/worker/runtimes.json","utf8"));
const pins={opentofu:["1.13.8","tofu","aa4382a5eefcf026309d07398ea803f97b9218a17d940999a938be22bfe9d73f"],terraform:["1.17.1","terraform","4638628058e71a55212b1c563712eac03b902128f40740f3dc8174df12135061"]};
const pkgs={null:["3.2.4","9d32ac3619cfc93eb3c4f423492a8e0f79db05fec58e449dee9b2d5873d5f69f"],aws:["6.41.0","4e8de5f3fcc3e3f41b6703ad4c0fa62175d0c29afcf56ac82eb16c604bb6dafa"]};
const fail=()=>{throw Error("Invalid pinned native fixture manifest")};
if(m.schema_version!==1||m.architecture!=="linux_amd64"||m.runtimes.length!==2||new Set(m.runtimes.map(r=>r.engine)).size!==2)fail();
for(const r of m.runtimes){const p=pins[r.engine],a=r.engine_archive;
if(!p||r.version!==p[0]||r.arch!=="linux_amd64"||r.base_image_ref!=="debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2"||r.git_package_version!=="1:2.47.3-0+deb13u1"||a?.sha256!==p[2]||a.member!==p[1])fail();
const url=r.engine==="terraform"?`https://releases.hashicorp.com/terraform/$${p[0]}/terraform_$${p[0]}_linux_amd64.zip`:`https://github.com/opentofu/opentofu/releases/download/v$${p[0]}/tofu_$${p[0]}_linux_amd64.zip`;
if(a.url!==url||r.url!==a.url||r.archive_sha256!==a.sha256||r.test_provider_packages.length!==2)fail();
const registry=r.engine==="terraform"?"registry.terraform.io":"registry.opentofu.org"; const seen=new Set();
for(const x of r.test_provider_packages){const name=x.source_addresses?.[0]?.split("/").at(-1),q=pkgs[name];
if(!q||seen.has(name)||x.version!==q[0]||x.sha256!==q[1]||x.arch!=="linux_amd64"||JSON.stringify(x.source_addresses)!==JSON.stringify([`$${registry}/hashicorp/$${name}`])||x.member!==`terraform-provider-$${name}_v$${q[0]}_x5`||x.archive_url!==`https://releases.hashicorp.com/terraform-provider-$${name}/$${q[0]}/terraform-provider-$${name}_$${q[0]}_linux_amd64.zip`)fail(); seen.add(name);}}
const run=args=>{const p=spawnSync("docker",args,{stdio:"inherit"});if(p.error)throw p.error;if(p.status!==0)process.exit(p.status??1)};
for(const r of m.runtimes){const a=r.engine_archive,tag=`cosmoformer-native-tests:$${r.engine}-$${r.version}`;
run(["build","--platform=linux/amd64","--target=native-tests","-f","deploy/worker/Dockerfile",...Object.entries({ENGINE:r.engine,ENGINE_VERSION:r.version,ENGINE_URL:a.url,ENGINE_SHA256:a.sha256,ENGINE_MEMBER:a.member,BASE_IMAGE:r.base_image_ref}).flatMap(([k,v])=>["--build-arg",`$${k}=$${v}`]),"-t",tag,"."]);
run(["run","--rm","--platform=linux/amd64","--network=none","--read-only","--tmpfs","/tmp:rw,exec,nosuid,nodev,size=1g",...Object.entries({COSMOFORMER_TEST_NATIVE:"1",COSMOFORMER_TEST_NATIVE_ENGINE:r.engine,COSMOFORMER_TEST_NATIVE_VERSION:r.version,COSMOFORMER_TEST_NATIVE_BINARY:`/usr/local/bin/$${a.member}`,COSMOFORMER_TEST_NATIVE_PROFILE_FILE:"/fixtures/runtimes.json",COSMOFORMER_TEST_NATIVE_PACKAGE_DIR:"/fixtures/provider-mirror"}).flatMap(([k,v])=>["-e",`$${k}=$${v}`]),tag]);}
endef

test-native: export COSMOFORMER_NATIVE_TEST_SCRIPT = $(NATIVE_TEST_SCRIPT)
test-native:
	@node --input-type=module -e "$$COSMOFORMER_NATIVE_TEST_SCRIPT"

test-scripts:
	COSMOFORMER_GO=$(GO) node --test scripts/merge-go-coverage.test.mjs scripts/native-make.test.mjs web/scripts/contract.test.mjs

worker-image:
	docker build --quiet --platform linux/amd64 --target worker -f deploy/worker/Dockerfile -t cosmoformer-worker:local .

test-coverage: test-go-coverage test-web-coverage test-contract-coverage

test-contract-coverage:
	mkdir -p .build/coverage/contract
	COSMOFORMER_GO=$(GO) node --test --experimental-test-coverage --test-coverage-include=web/scripts/contract.mjs --test-reporter=spec --test-reporter-destination=stdout --test-reporter=lcov --test-reporter-destination=.build/coverage/contract/lcov.info web/scripts/contract.test.mjs

# Includes acceptance calls in the same profile as unit tests; never use the dev DB.
test-go-coverage: test-linux-coverage
	mkdir -p .build/coverage
	@set -eu; cover_dir=$$(mktemp -d "$(CURDIR)/.build/coverage/go-raw.XXXXXX"); \
	trap 'rm -rf "$$cover_dir"' EXIT; \
	COSMOFORMER_TEST_COVER_DIR="$$cover_dir" $(GO) test -race -count=1 -tags=integration -covermode=atomic -coverpkg=./internal/...,./cmd/... ./internal/... ./cmd/... ./tests/acceptance -args -test.gocoverdir="$$cover_dir"; \
	$(GO) tool covdata textfmt -i="$$cover_dir" -o=.build/coverage/go.host.out
	mkdir -p .build/coverage/scripts
	node --test --experimental-test-coverage --test-reporter=spec --test-reporter-destination=stdout --test-reporter=lcov --test-reporter-destination=.build/coverage/scripts/lcov.info scripts/merge-go-coverage.test.mjs
	node scripts/merge-go-coverage.mjs .build/coverage/go.out .build/coverage/go.host.out .build/coverage/linux/source.out .build/coverage/linux/worker.out .build/coverage/linux/clock.out .build/coverage/linux/command.out
	$(GO) tool cover -func=.build/coverage/go.out
	$(GO) tool cover -html=.build/coverage/go.out -o=.build/coverage/go.html

test-linux-coverage:
	docker build --quiet --platform linux/amd64 --target source-coverage -f deploy/worker/Dockerfile -t cosmoformer-worker-source-coverage:local .
	mkdir -p .build/coverage
	@set -eu; cover_dir=$$(mktemp -d "$(CURDIR)/.build/coverage-linux.XXXXXX"); \
	trap 'rm -rf "$$cover_dir"' EXIT; \
	docker run --rm --network none --platform linux/amd64 --user "$$(id -u):$$(id -g)" --mount "type=bind,source=$$cover_dir,target=/coverage" cosmoformer-worker-source-coverage:local; \
	mkdir -p .build/coverage/linux; \
	cp "$$cover_dir"/*.out .build/coverage/linux/

test-web-unit: contract
	cd web && npm run test:unit

test-web-coverage: contract
	cd web && npm run test:coverage

browser-server:
	COSMOFORMER_TEST_BROWSER=1 $(GO) test -count=1 -tags=integration ./tests/acceptance -run '^TestBrowserServer$$' -timeout 10m -v

test-web: contract
	mkdir -p .build/browser-tmp
	cd web && npm run check && npm run test:coverage && npm run build && TMPDIR=$(CURDIR)/.build/browser-tmp npm test

# Checks build local test images; none publishes or deploys resources.
check:
	$(MAKE) test-unit
	$(MAKE) test-integration
	$(MAKE) test-source
	$(MAKE) test-scripts
	$(MAKE) test-native
	$(MAKE) test-web
