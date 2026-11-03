APP       := importer-api
VERSION   ?= 1.5.0
IMAGE     := ghcr.io/c88lopez/$(APP):$(VERSION)
NAMESPACE := importer-api

.PHONY: tunnel dev-setup run test build image migrate-local migration release migrate dashboard deploy status logs restart rollout undeploy

## --- local ---
# Local runs use the cluster's Postgres and RabbitMQ through `make tunnel`,
# with credentials read from their Secrets (each value is fetched once, and
# only by targets that use it). Override with the LOCAL_* variables.
#
#   run, migrate-local   the real `app` database and the `/` vhost
#   test                 importer_test and the importer-test vhost, because
#                        tests truncate tables and delete exchanges
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

run: export DATABASE_URL = $(LOCAL_DATABASE_URL)
run: export RABBITMQ_URL = $(LOCAL_RABBITMQ_URL)
run: ## run the API on your machine against the cluster's Postgres and RabbitMQ (needs make tunnel)
	go run .

test: export TEST_DATABASE_URL = $(LOCAL_TEST_DATABASE_URL)
test: export TEST_RABBITMQ_URL = $(LOCAL_TEST_RABBITMQ_URL)
test: ## unit tests, plus Postgres and RabbitMQ tests in the cluster's test database and vhost (needs make tunnel)
	go test ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(APP) .

image: ## build the image locally to try it (CI builds the one the cluster uses)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# This is the real `app` database: an unreleased migration applied here is
# live before its release. make migrate (the Job) is the normal path.
MIGRATE ?= up
migrate-local: export DATABASE_URL = $(LOCAL_DATABASE_URL)
migrate-local: ## run goose from your machine on the cluster's app database: make migrate-local [MIGRATE=status]
	go run . migrate $(MIGRATE)

migration: ## add an empty migration file: make migration NAME=add_items_owner
	go run . migrate create $(NAME)

## --- kubernetes ---
release: ## tag a version; CI builds and pushes it: make release VERSION=1.4.0
	git tag v$(VERSION)
	git push origin v$(VERSION)

# The Job's pod has no fixed name, so poll its conditions until it either
# completes or fails (activeDeadlineSeconds turns a hang into a failure).
JOB_STATE = kubectl -n $(NAMESPACE) get job $(APP)-migrate -o jsonpath='{.status.conditions[?(@.status=="True")].type}'

migrate: ## run pending migrations in the cluster with the $(VERSION) image; stops if they fail
	kubectl -n $(NAMESPACE) delete job $(APP)-migrate --ignore-not-found
	sed 's|image: ghcr.io/.*|image: $(IMAGE)|' k8s/jobs/migrate.yaml | kubectl apply -f -
	@echo "waiting for job/$(APP)-migrate..."
	@until $(JOB_STATE) | grep -qwE 'Complete|Failed'; do sleep 2; done
	-kubectl -n $(NAMESPACE) logs job/$(APP)-migrate
	@$(JOB_STATE) | grep -qw Complete || { echo "migration failed, nothing was deployed"; exit 1; }

# Grafana's sidecar loads any ConfigMap labeled grafana_dashboard, from any
# namespace, into the folder named by grafana_folder. Edit the dashboard in
# Grafana, export it (Share > Export > JSON), save it over the file, re-run this.
dashboard: ## load k8s/dashboards/importer-api.json into Grafana (no release needed)
	kubectl -n $(NAMESPACE) create configmap $(APP)-dashboard \
	  --from-file=importer-api.json=k8s/dashboards/importer-api.json --dry-run=client -o yaml \
	  | kubectl label --local -f - grafana_dashboard=1 -o yaml \
	  | kubectl annotate --local -f - grafana_folder=Services -o yaml \
	  | kubectl apply --server-side -f -

deploy: migrate ## migrate, then apply all manifests and the dashboard (the image comes from ghcr.io)
	kubectl apply -f k8s/
	$(MAKE) dashboard
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

status: ## show everything running in the namespace
	kubectl -n $(NAMESPACE) get all,configmap -o wide

logs: ## stream logs from all pods
	kubectl -n $(NAMESPACE) logs -l app=$(APP) -f --prefix

restart: ## recreate pods (e.g. after changing the ConfigMap)
	kubectl -n $(NAMESPACE) rollout restart deployment/$(APP)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

rollout: migrate ## migrate, then switch to a version CI already pushed: make rollout VERSION=1.4.0
	kubectl -n $(NAMESPACE) set image deployment/$(APP) $(APP)=$(IMAGE)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

undeploy: ## delete everything (the namespace and all it contains)
	kubectl delete -f k8s/ --ignore-not-found
