# Everyday commands. `make help` lists them.
.DEFAULT_GOAL := help
SERVICES := product-service order-service payment-service cart-service web-service

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^[a-z0-9-]+:.*## /{printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: test
test: ## Format check, vet and race-detector tests for every service
	@for s in $(SERVICES); do \
	  echo "== $$s"; \
	  (cd $$s && test -z "$$(find . -name node_modules -prune -o -name '*.go' -print | xargs gofmt -l)" || { echo "gofmt needed"; exit 1; }) && \
	  (cd $$s && go vet ./... && go test -race -count=1 ./...) || exit 1; \
	done

.PHONY: up
up: ## Build and start all five services with Docker Compose
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
outage: ## Demo: stop Product, then Payment, mid-order and watch recovery
	./scripts/outage-demo.sh

.PHONY: oversell
oversell: ## Demo: 20 customers x 2 clicks for 3 lamps; nothing oversold
	./scripts/oversell-demo.sh

.PHONY: web
web: ## Build the React front end into the web gateway (needs Node 22)
	cd web-service/frontend && npm ci --no-audit --no-fund && npm run build

.PHONY: web-dev
web-dev: ## Front end with hot reload on :5173, using the stack from `make up`
	cd web-service/frontend && npm run dev

.PHONY: k8s-check
k8s-check: ## Validate the Kubernetes manifests (no cluster needed)
	python3 scripts/check-k8s.py

.PHONY: tf-plan
tf-plan: ## terraform plan with throw-away credentials: no AWS account needed
	cd infra/terraform && terraform init -backend=false -input=false && terraform plan -input=false -lock=false -var offline=true
