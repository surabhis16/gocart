# Everyday commands. `make help` lists them.
.DEFAULT_GOAL := help
SERVICES := product-service order-service

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^[a-z0-9-]+:.*## /{printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: test
test: ## Format check, vet and race-detector tests for both services
	@for s in $(SERVICES); do \
	  echo "== $$s"; \
	  (cd $$s && test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }) && \
	  (cd $$s && go vet ./... && go test -race -count=1 ./...) || exit 1; \
	done

.PHONY: up
up: ## Build and start both services with Docker Compose
	docker compose up -d --build --wait

.PHONY: up-cassandra
up-cassandra: ## Same, with Cassandra as the Order service's event store
	docker compose -f docker-compose.yml -f docker-compose.cassandra.yml up -d --build --wait --wait-timeout 300

.PHONY: down
down: ## Stop everything and delete its data
	docker compose -f docker-compose.yml -f docker-compose.cassandra.yml down -v --remove-orphans

.PHONY: smoke
smoke: ## End-to-end checks against a fresh running stack
	./scripts/smoke.sh

.PHONY: outage
outage: ## Demo: stop the Product service mid-order and watch recovery
	./scripts/outage-demo.sh

.PHONY: k8s-check
k8s-check: ## Validate the Kubernetes manifests (no cluster needed)
	python3 scripts/check-k8s.py

.PHONY: tf-plan
tf-plan: ## terraform plan with throw-away credentials: no AWS account needed
	cd infra/terraform && terraform init -backend=false -input=false && terraform plan -input=false -lock=false -var offline=true
