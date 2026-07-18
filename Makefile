-include .env

DATABASE_URL ?= postgres://semantic_search:semantic_search@localhost:5432/semantic_search?sslmode=disable
GOOSE_VERSION ?= v3.27.0

# RustFS does not include an S3 administration client. Run minio/mc through the
# profile-gated storage-cli service as a disposable container for bucket tasks.
STORAGE_CLI := docker compose run --rm --no-deps --entrypoint /bin/sh storage-cli

.PHONY: db-up db-down db-psql db-reset-dev ingestion-retry migrate-up migrate-down migrate-status test-integration tools storage-up storage-down storage-configure storage-events storage-status stack-down

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose rm --stop --force postgres

storage-up:
	docker compose up -d --wait --remove-orphans rustfs
	$(MAKE) storage-configure

storage-down:
	docker compose rm --stop --force rustfs

# RustFS loads the environment-managed webhook target during startup. Verify
# that it is online before creating a bucket rule that references its ARN.
storage-configure:
	@set -a; [ ! -f .env ] || . ./.env; set +a; \
		: "$${S3_ENDPOINT:=http://127.0.0.1:9000}"; \
		: "$${S3_ACCESS_KEY:=minioadmin}"; \
		: "$${S3_SECRET_KEY:=minioadmin}"; \
		curl --fail --silent --show-error --aws-sigv4 'aws:amz:us-east-1:s3' \
			--user "$$S3_ACCESS_KEY:$$S3_SECRET_KEY" \
			--header 'Content-Type: application/json' \
			--request PUT \
			--data '{"notify_enabled":true,"audit_enabled":false}' \
			"$${S3_ENDPOINT%/}/rustfs/admin/v3/module-switches" >/dev/null; \
		targets=$$(curl --fail --silent --show-error --aws-sigv4 'aws:amz:us-east-1:s3' \
			--user "$$S3_ACCESS_KEY:$$S3_SECRET_KEY" \
			"$${S3_ENDPOINT%/}/rustfs/admin/v3/target/list"); \
		python3 -c 'import json,sys; targets=json.loads(sys.argv[1]).get("notification_endpoints", []); sys.exit(0 if any(t.get("account_id") == "primary" and t.get("service") == "webhook" and t.get("status") == "online" for t in targets) else "RustFS primary webhook target is not online")' "$$targets"
	$(STORAGE_CLI) -ec 'until mc alias set local "$$S3_ENDPOINT" "$$S3_ACCESS_KEY" "$$S3_SECRET_KEY"; do sleep 1; done; mc mb --ignore-existing local/"$$S3_BUCKET"; mc event remove local/"$$S3_BUCKET" --force || true; mc event add local/"$$S3_BUCKET" arn:rustfs:sqs:us-east-1:primary:webhook --event put --prefix incoming/'

storage-events:
	$(STORAGE_CLI) -c 'mc alias set local "$$S3_ENDPOINT" "$$S3_ACCESS_KEY" "$$S3_SECRET_KEY" && mc event ls local/"$$S3_BUCKET"'

storage-status:
	@set -a; [ ! -f .env ] || . ./.env; set +a; \
		: "$${S3_ENDPOINT:=http://127.0.0.1:9000}"; \
		: "$${S3_ACCESS_KEY:=minioadmin}"; \
		: "$${S3_SECRET_KEY:=minioadmin}"; \
		curl --fail --silent --show-error --aws-sigv4 'aws:amz:us-east-1:s3' \
			--user "$$S3_ACCESS_KEY:$$S3_SECRET_KEY" \
			"$${S3_ENDPOINT%/}/rustfs/admin/v3/target/list" | python3 -m json.tool
	$(MAKE) storage-events

stack-down:
	docker compose down --remove-orphans

db-psql:
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'

# Failed jobs are not retried by duplicate object notifications. Requeue one
# explicitly so retry limits remain meaningful and other job states are safe.
ingestion-retry:
	@case "$(JOB_ID)" in ''|*[!0-9]*) echo 'usage: make ingestion-retry JOB_ID=<failed-job-id>' >&2; exit 2;; esac
	go run ./cmd/retry-ingestion -id "$(JOB_ID)"

db-reset-dev:
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -v ON_ERROR_STOP=1 -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"'
	$(MAKE) migrate-up

migrate-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

migrate-status:
	goose -dir migrations postgres "$(DATABASE_URL)" status

test-integration:
	go test -count=1 -tags=integration ./internal/database

tools:
	go install github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)
