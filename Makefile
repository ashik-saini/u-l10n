.PHONY: test test-report-dep test-report gen-wire pre-commit install clean \
        db-local db-local-stop db-test db-migrate run-local

# Local development defaults. Override on the command line, e.g.
#   make run-local DATABASECONFIG_DATABASENAME=u_l10n_scratch
DATABASECONFIG_TYPE         ?= postgres
DATABASECONFIG_HOST         ?= localhost
DATABASECONFIG_PORT         ?= 5432
DATABASECONFIG_USER         ?= $(USER)
DATABASECONFIG_PASSWORD     ?= postgres
DATABASECONFIG_DATABASENAME ?= u_l10n_local

# Scratch databases. db-test is wiped by every schema-test run; db-migrate
# drops and recreates its own to prove a from-empty migration.
TEST_DATABASE_NAME    ?= u_l10n_test
MIGRATE_DATABASE_NAME ?= u_l10n_migrate_test

LOCAL_ENV = \
	SERVICECONFIG_ENV=local \
	DATABASECONFIG_TYPE=$(DATABASECONFIG_TYPE) \
	DATABASECONFIG_HOST=$(DATABASECONFIG_HOST) \
	DATABASECONFIG_PORT=$(DATABASECONFIG_PORT) \
	DATABASECONFIG_USER=$(DATABASECONFIG_USER) \
	DATABASECONFIG_PASSWORD=$(DATABASECONFIG_PASSWORD) \
	DATABASECONFIG_DATABASENAME=$(DATABASECONFIG_DATABASENAME)

test:
	go test -v -cover ./...

test-report-dep:
	mkdir -p ./reports

test-report: test-report-dep
	go test -v -coverprofile=./reports/coverage.out ./... | tee ./reports/test.out
	cat ./reports/test.out | go-junit-report > ./reports/test-report.xml

# Regenerates wire_gen.go. Requires the wire binary:
#   go install github.com/google/wire/cmd/wire@latest
# Note: the wire *library* is pinned to v0.5.0 in go.mod to match the other
# services; the *generator* must be newer than v0.6.0 to parse modern Go.
# wire_gen.go does not import wire at runtime, so the two versions are
# independent.
gen-wire:
	# Root package only: wire ./... fails on packages with no wire directives.
	wire .

pre-commit: gen-wire
	go mod tidy
	go vet ./...
	go fmt ./...

install:
	go install

clean:
	rm -rf ./reports ./u-l10n

# --- Local development -------------------------------------------------------

# Starts the Homebrew PostgreSQL service and creates the local database if it
# does not already exist. Idempotent.
db-local:
	@brew services start postgresql@14 >/dev/null 2>&1 || true
	@until pg_isready -q -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT); do sleep 1; done
	@psql -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT) -lqt \
		| cut -d \| -f 1 | grep -qw $(DATABASECONFIG_DATABASENAME) \
		|| createdb -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT) $(DATABASECONFIG_DATABASENAME)
	@echo "postgres ready: $(DATABASECONFIG_DATABASENAME)"

db-local-stop:
	@brew services stop postgresql@14

# Scratch database for the schema tests. They DROP and recreate the public
# schema on every run, so never point TEST_DATABASE_URL at anything you value.
db-test:
	@brew services start postgresql@14 >/dev/null 2>&1 || true
	@until pg_isready -q -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT); do sleep 1; done
	@psql -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT) -lqt \
		| cut -d \| -f 1 | grep -qw $(TEST_DATABASE_NAME) \
		|| createdb -h $(DATABASECONFIG_HOST) -p $(DATABASECONFIG_PORT) $(TEST_DATABASE_NAME)
	@echo "test database ready: $(TEST_DATABASE_NAME)"

# Applies the migrations with the real Flyway, the way the deploy pipeline
# does, against a throwaway database. The schema tests replay the same files
# through database/sql; this target is what proves Flyway itself is happy with
# the filename convention and ordering.
#
#   brew install flyway
db-migrate: db-test
	@dropdb --if-exists $(MIGRATE_DATABASE_NAME)
	@createdb $(MIGRATE_DATABASE_NAME)
	flyway -url=jdbc:postgresql://$(DATABASECONFIG_HOST):$(DATABASECONFIG_PORT)/$(MIGRATE_DATABASE_NAME) \
		-user=$(DATABASECONFIG_USER) -password=$(DATABASECONFIG_PASSWORD) \
		-locations=filesystem:.db migrate

run-local: db-local
	$(LOCAL_ENV) go run .

# Loads the committed u-mobile tree. Add DRY_RUN=--dry-run to rehearse.
U_MOBILE_PATH ?= ../../FE/u-mobile
seed-local: db-local
	$(LOCAL_ENV) go run . seed-from-files --root $(U_MOBILE_PATH) --actor $(USER)@you.co $(DRY_RUN)
