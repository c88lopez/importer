APP       := importer-api
VERSION   ?= 1.0.0
IMAGE     := ghcr.io/c88lopez/$(APP):$(VERSION)
NAMESPACE := importer-api

.PHONY: run test build image release deploy status logs restart rollout undeploy

## --- local ---
run: ## run the API on your machine
	go run .

test:
	go test ./...

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(APP) .

image: ## build the image locally to try it (CI builds the one the cluster uses)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## --- kubernetes ---
release: ## tag a version; CI builds and pushes it: make release VERSION=1.1.0
	git tag v$(VERSION)
	git push origin v$(VERSION)

deploy: ## apply all manifests (the image comes from ghcr.io)
	kubectl apply -f k8s/
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

status: ## show everything running in the namespace
	kubectl -n $(NAMESPACE) get all,configmap -o wide

logs: ## stream logs from all pods
	kubectl -n $(NAMESPACE) logs -l app=$(APP) -f --prefix

restart: ## recreate pods (e.g. after changing the ConfigMap)
	kubectl -n $(NAMESPACE) rollout restart deployment/$(APP)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

rollout: ## switch to a version CI already pushed: make rollout VERSION=1.1.0
	kubectl -n $(NAMESPACE) set image deployment/$(APP) $(APP)=$(IMAGE)
	kubectl -n $(NAMESPACE) rollout status deployment/$(APP)

undeploy: ## delete everything (the namespace and all it contains)
	kubectl delete -f k8s/ --ignore-not-found
