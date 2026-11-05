# Both services build from this repo and share a version: one tag releases
# both images, and `make deploy` rolls out both. The version deployed is the
# newTag in k8s/kustomization.yaml; VERSION defaults to it.
VERSION   ?= $(shell sed -n "s/^ *newTag: *//p" k8s/kustomization.yaml | head -1)
REGISTRY  := ghcr.io/c88lopez
NAMESPACE := importer
API_IMAGE    := $(REGISTRY)/importer-api:$(VERSION)
WORKER_IMAGE := $(REGISTRY)/importer-worker:$(VERSION)

.PHONY: tunnel dev-setup run-api run-worker test build image migrate-local migration release secrets migrate dashboard deploy rollout status logs restart undeploy

## --- local ---
# Local runs use the cluster's Postgres and RabbitMQ through `make tunnel`,
# with credentials read from their Secrets (each value is fetched once, and
# only by targets that use it). Override with the LOCAL_* variables.
#
#   run-*, migrate-local   the real `app` database and the `/` vhost
#   test                   importer_test and the importer-test vhost, because
#                          tests truncate tables and delete exchanges
secret = $(shell kubectl -n default get secret $(1) -o jsonpath='{.data.$(2)}' | base64 -d)
PG_USER = $(eval PG_USER := $(call secret,postgres-app,username))$(PG_USER)
PG_PASS = $(eval PG_PASS := $(call secret,postgres-app,password))$(PG_PASS)
MQ_USER = $(eval MQ_USER := $(call secret,rabbitmq-default-user,username))$(MQ_USER)
MQ_PASS = $(eval MQ_PASS := $(call secret,rabbitmq-default-user,password))$(MQ_PASS)
TEST_VHOST := importer-test

LOCAL_DATABASE_URL      ?= postgres://$(PG_USER):$(PG_PASS)@localhost:5433/app
LOCAL_TEST_DATABASE_URL ?= postgres://$(PG_USER):$(PG_PASS)@localhost:5433/importer_test
LOCAL_RABBITMQ_URL      ?= amqp://$(MQ_USER):$(MQ_PASS)@localhost:5673/
LOCAL_TEST_RABBITMQ_URL ?= amqp://$(MQ_USER):$(MQ_PASS)@localhost:5673/$(TEST_VHOST)

# Leave it running in its own terminal. Ports 5433 and 5673, so it doesn't
# clash with a forward of Postgres on 5432 you may already have.
tunnel: ## forward the cluster's Postgres (5433) and RabbitMQ (5673, UI on 15673) to localhost
	@trap 'kill 0' INT TERM EXIT; \
	kubectl -n default port-forward svc/postgres-rw 5433:5432 & \
	kubectl -n default port-forward svc/rabbitmq 5673:5672 15673:15672 & \
	wait

dev-setup: ## once: create the test database and the test vhost in the cluster
	kubectl apply -f k8s/dev/
	kubectl -n default exec rabbitmq-server-0 -c rabbitmq -- rabbitmqctl add_vhost $(TEST_VHOST) || true
	kubectl -n default exec rabbitmq-server-0 -c rabbitmq -- rabbitmqctl set_permissions -p $(TEST_VHOST) "$(MQ_USER)" ".*" ".*" ".*"

run-api: export DATABASE_URL = $(LOCAL_DATABASE_URL)
run-api: export RABBITMQ_URL = $(LOCAL_RABBITMQ_URL)
run-api: ## run the API on your machine against the cluster (needs make tunnel): :8080, metrics on :9464
	go run ./cmd/api

# Port 9465: the API already serves its metrics on 9464.
run-worker: export DATABASE_URL = $(LOCAL_DATABASE_URL)
run-worker: export RABBITMQ_URL = $(LOCAL_RABBITMQ_URL)
run-worker: ## run the worker on your machine against the cluster (needs make tunnel): probes and metrics on :9465
	PORT=9465 go run ./cmd/worker

# -p 1: packages share the test database, so they run one at a time.
test: export TEST_DATABASE_URL = $(LOCAL_TEST_DATABASE_URL)
test: export TEST_RABBITMQ_URL = $(LOCAL_TEST_RABBITMQ_URL)
test: ## unit tests, plus Postgres and RabbitMQ tests in the cluster's test database and vhost (needs make tunnel)
	go test -p 1 ./...

build: ## build both binaries into bin/
	go build -ldflags "-X main.version=$(VERSION)" -o bin/importer-api ./cmd/api
	go build -ldflags "-X main.version=$(VERSION)" -o bin/importer-worker ./cmd/worker

APP ?= api
image: ## build one image locally to try it: make image [APP=worker] (CI builds the ones the cluster uses)
	docker build --build-arg APP=$(APP) --build-arg VERSION=$(VERSION) -t $(REGISTRY)/importer-$(APP):$(VERSION) .

# This is the real `app` database: an unreleased migration applied here is
# live before its release. make migrate (the Job) is the normal path.
MIGRATE ?= up
migrate-local: export DATABASE_URL = $(LOCAL_DATABASE_URL)
migrate-local: ## run goose from your machine on the cluster's app database: make migrate-local [MIGRATE=status]
	go run ./cmd/api migrate $(MIGRATE)

migration: ## add an empty migration file: make migration NAME=add_items_owner
	go run ./cmd/api migrate create $(NAME)

## --- kubernetes ---
release: ## tag a version; CI builds and pushes both images: make release VERSION=1.6.0
	git tag v$(VERSION)
	git push origin v$(VERSION)

# Each service gets its own Secret with only what it uses. The worker uses
# the same database user as the API: it updates the API's items table.
# --server-side: a client-side `kubectl apply` would keep a plain-text copy of
# the URLs (with their passwords) in the last-applied-configuration annotation.
secrets: ## create or update both Secrets from the cluster's Postgres, RabbitMQ and Redis Secrets
	kubectl apply -f k8s/00-namespace.yaml
	kubectl -n $(NAMESPACE) create secret generic importer-api-deps \
	  --from-literal=DATABASE_URL="$$(kubectl -n default get secret postgres-app -o jsonpath='{.data.fqdn-uri}' | base64 -d)" \
	  --from-literal=RABBITMQ_URL="$$(kubectl -n default get secret rabbitmq-default-user -o jsonpath='{.data.connection_string}' | base64 -d)" \
	  --from-literal=REDIS_URL="redis://:$$(kubectl -n default get secret redis-secret -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d)@redis.default.svc.cluster.local:6379/0" \
	  --dry-run=client -o yaml | kubectl apply --server-side -f -
	kubectl -n $(NAMESPACE) create secret generic importer-worker-deps \
	  --from-literal=DATABASE_URL="$$(kubectl -n default get secret postgres-app -o jsonpath='{.data.fqdn-uri}' | base64 -d)" \
	  --from-literal=RABBITMQ_URL="$$(kubectl -n default get secret rabbitmq-default-user -o jsonpath='{.data.connection_string}' | base64 -d)" \
	  --dry-run=client -o yaml | kubectl apply --server-side -f -

# The Job's pod has no fixed name, so poll its conditions until it either
# completes or fails (activeDeadlineSeconds turns a hang into a failure).
JOB_STATE = kubectl -n $(NAMESPACE) get job importer-api-migrate -o jsonpath='{.status.conditions[?(@.status=="True")].type}'

migrate: ## run pending migrations in the cluster with the $(VERSION) image; stops if they fail
	kubectl -n $(NAMESPACE) delete job importer-api-migrate --ignore-not-found
	sed 's|image: ghcr.io/.*|image: $(API_IMAGE)|' k8s/jobs/migrate.yaml | kubectl apply -f -
	@echo "waiting for job/importer-api-migrate..."
	@until $(JOB_STATE) | grep -qwE 'Complete|Failed'; do sleep 2; done
	-kubectl -n $(NAMESPACE) logs job/importer-api-migrate
	@$(JOB_STATE) | grep -qw Complete || { echo "migration failed, nothing was deployed"; exit 1; }

# Grafana's sidecar loads any ConfigMap labeled grafana_dashboard, from any
# namespace. k8s/kustomization.yaml makes one per file in k8s/dashboards/
# (<name>-dashboard). Edit a dashboard in Grafana, export it
# (Share > Export > JSON), save it over the file, re-run this.
dashboard: ## load every dashboard in k8s/dashboards/ into Grafana (no release needed)
	kubectl apply -k k8s/ -l grafana_dashboard=1

# Order matters: the migration first, so the API and worker of a version
# always find the schema they expect. The Job is left out here (make migrate
# already ran it), and k8s/dev/ isn't in the kustomization.
deploy: migrate ## migrate, then roll out the API and the worker and load the dashboards
	kubectl apply -k k8s/ -l 'component!=migrate'
	kubectl -n $(NAMESPACE) rollout status deployment/importer-api
	kubectl -n $(NAMESPACE) rollout status deployment/importer-worker

# Changes the cluster only: bump newTag in k8s/kustomization.yaml too, or the
# next make deploy puts the old version back.
rollout: migrate ## migrate, then switch both services to a version CI already pushed: make rollout VERSION=1.6.0
	kubectl -n $(NAMESPACE) set image deployment/importer-api importer-api=$(API_IMAGE)
	kubectl -n $(NAMESPACE) set image deployment/importer-worker importer-worker=$(WORKER_IMAGE)
	kubectl -n $(NAMESPACE) rollout status deployment/importer-api
	kubectl -n $(NAMESPACE) rollout status deployment/importer-worker

status: ## show everything running in the namespace
	kubectl -n $(NAMESPACE) get all,configmap -o wide

logs: ## stream logs from one service: make logs [APP=worker]
	kubectl -n $(NAMESPACE) logs -l app=importer-$(APP) -f --prefix

restart: ## recreate the pods of both services (e.g. after changing a ConfigMap or Secret)
	kubectl -n $(NAMESPACE) rollout restart deployment/importer-api deployment/importer-worker
	kubectl -n $(NAMESPACE) rollout status deployment/importer-api
	kubectl -n $(NAMESPACE) rollout status deployment/importer-worker

# Keeps the namespace and the Secrets, so a later make deploy just works.
undeploy: ## delete both services (not the namespace)
	kubectl delete -f k8s/api/ -f k8s/worker/ --ignore-not-found
