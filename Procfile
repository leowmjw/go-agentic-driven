logs: victoria-logs-prod -httpListenAddr=127.0.0.1:9428 -storageDataPath=.local/victorialogs
api: go run ./cmd/server
web: npm --prefix frontend run dev -- --host 127.0.0.1
deploy: go run ./cmd/factory serve
