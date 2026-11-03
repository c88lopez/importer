APP       := importer-api
VERSION   ?= 1.2.0
IMAGE     := ghcr.io/c88lopez/$(APP):$(VERSION)
NAMESPACE := importer-api

.PHONY: db-up db-down run test build image migrate-local migration release migrate deploy status logs restart rollout undeploy

## --- local ---
# Defaults point at the compose.yaml Postgres; set DATABASE_URL to override.
export DATABASE_URL      ?= postgres://importer:importer@localhost:5432/importer?sslmode=disable
export TEST_DATABASE_URL ?= postgres://importer:importer@localhost:5432/importer_test?sslmode=disable

db-up: ## start the local Postgres and wait until it accepts connections
	docker compose up -d --wait postgres

db-down: ## stop the local Postgres (data is kept)
	docker compose down

run: ## run the API on your machine (needs make db-up and make migrate-local first)
	go run .

test: ## unit tests, plus Postgres tests when the local database is up
	go test ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(APP) .

image: ## build the image locally to try it (CI builds the one the cluster uses)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

MIGRATE ?= up
migrate-local: ## migrate the local database: make migrate-local [MIGRATE=status|down]
	go run . migrate $(MIGRATE)

migration: ## add an empty migration file: make migration NAME=add_items_owner
	go run . migrate create $(NAME)

## --- kubernetes ---
release: ## tag a version; CI builds and pushes it: make release VERSION=1.2.0
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

deploy: migrate ## migrate, then apply all manifests (the image comes from ghcr.io)
	kubectl apply -f k8s/
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

status: ## show everything running in the namespace
	kubectl -n $(NAMESPACE) get all,configmap -o wide

logs: ## stream logs from all pods
	kubectl -n $(NAMESPACE) logs -l app=$(APP) -f --prefix

restart: ## recreate pods (e.g. after changing the ConfigMap)
	kubectl -n $(NAMESPACE) rollout restart deployment/$(APP)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

rollout: migrate ## migrate, then switch to a version CI already pushed: make rollout VERSION=1.2.0
	kubectl -n $(NAMESPACE) set image deployment/$(APP) $(APP)=$(IMAGE)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

undeploy: ## delete everything (the namespace and all it contains)
	kubectl delete -f k8s/ --ignore-not-found
