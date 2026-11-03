APP       := importer-worker
VERSION   ?= 1.0.0
IMAGE     := ghcr.io/c88lopez/$(APP):$(VERSION)
NAMESPACE := $(APP)

.PHONY: tunnel dev-setup run test build image release secret deploy status logs restart undeploy

## --- local ---
# Local runs use the cluster's Postgres and RabbitMQ through `make tunnel`,
# with credentials read from their Secrets (each value is fetched once, and
# only by targets that use it). Override with the LOCAL_* variables.
#
#   run    the real `app` database and `/` vhost: it consumes the real queue,
#          sharing the messages with the cluster's workers
#   test   importer_worker_test and the importer-test vhost
secret = $(shell kubectl -n default get secret $(1) -o jsonpath='{.data.$(2)}' | base64 -d)
PG_USER = $(eval PG_USER := $(call secret,postgres-app,username))$(PG_USER)
PG_PASS = $(eval PG_PASS := $(call secret,postgres-app,password))$(PG_PASS)
MQ_USER = $(eval MQ_USER := $(call secret,rabbitmq-default-user,username))$(MQ_USER)
MQ_PASS = $(eval MQ_PASS := $(call secret,rabbitmq-default-user,password))$(MQ_PASS)
TEST_VHOST := importer-test

LOCAL_DATABASE_URL      ?= postgres://$(PG_USER):$(PG_PASS)@localhost:5433/app
LOCAL_TEST_DATABASE_URL ?= postgres://$(PG_USER):$(PG_PASS)@localhost:5433/importer_worker_test
LOCAL_RABBITMQ_URL      ?= amqp://$(MQ_USER):$(MQ_PASS)@localhost:5673/
LOCAL_TEST_RABBITMQ_URL ?= amqp://$(MQ_USER):$(MQ_PASS)@localhost:5673/$(TEST_VHOST)

# The same tunnel as importer-api's: run one of them, not both.
tunnel: ## forward the cluster's Postgres (5433) and RabbitMQ (5673, UI on 15673) to localhost
	@trap 'kill 0' INT TERM EXIT; \
	kubectl -n default port-forward svc/postgres-rw 5433:5432 & \
	kubectl -n default port-forward svc/rabbitmq 5673:5672 15673:15672 & \
	wait

dev-setup: ## once: create the test database and the test vhost in the cluster
	kubectl apply -f k8s/dev/
	kubectl -n default exec rabbitmq-server-0 -c rabbitmq -- rabbitmqctl add_vhost $(TEST_VHOST) || true
	kubectl -n default exec rabbitmq-server-0 -c rabbitmq -- rabbitmqctl set_permissions -p $(TEST_VHOST) "$(MQ_USER)" ".*" ".*" ".*"

# Port 9465: importer-api's make run already serves its metrics on 9464.
run: export DATABASE_URL = $(LOCAL_DATABASE_URL)
run: export RABBITMQ_URL = $(LOCAL_RABBITMQ_URL)
run: ## run the worker on your machine against the cluster (needs make tunnel)
	PORT=9465 go run .

test: export TEST_DATABASE_URL = $(LOCAL_TEST_DATABASE_URL)
test: export TEST_RABBITMQ_URL = $(LOCAL_TEST_RABBITMQ_URL)
test: ## unit tests, plus Postgres and RabbitMQ tests in the cluster's test database and vhost (needs make tunnel)
	go test ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(APP) .

image: ## build the image locally to try it (CI builds the one the cluster uses)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## --- kubernetes ---
release: ## tag a version; CI builds and pushes it: make release VERSION=1.0.0
	git tag v$(VERSION)
	git push origin v$(VERSION)

# Same database user as importer-api: the worker updates its items table.
secret: ## create or update the Secret from the cluster's Postgres and RabbitMQ Secrets
	kubectl apply -f k8s/00-namespace.yaml
	kubectl -n $(NAMESPACE) create secret generic $(APP)-deps \
	  --from-literal=DATABASE_URL="$$(kubectl -n default get secret postgres-app -o jsonpath='{.data.fqdn-uri}' | base64 -d)" \
	  --from-literal=RABBITMQ_URL="$$(kubectl -n default get secret rabbitmq-default-user -o jsonpath='{.data.connection_string}' | base64 -d)" \
	  --dry-run=client -o yaml | kubectl apply -f -

deploy: ## apply all manifests (the image comes from ghcr.io); needs make secret once
	kubectl apply -f k8s/
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

status: ## show everything running in the namespace
	kubectl -n $(NAMESPACE) get all,configmap -o wide

logs: ## stream logs from all pods
	kubectl -n $(NAMESPACE) logs -l app=$(APP) -f --prefix

restart: ## recreate pods (e.g. after changing the ConfigMap or Secret)
	kubectl -n $(NAMESPACE) rollout restart deployment/$(APP)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

undeploy: ## delete everything (the namespace and all it contains)
	kubectl delete -f k8s/ --ignore-not-found
