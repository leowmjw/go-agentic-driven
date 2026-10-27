logs: victoria-logs -httpListenAddr=127.0.0.1:9428 -storageDataPath=data/victoria-logs
api: go run ./cmd/server
web: npm --prefix frontend run dev -- --host 127.0.0.1
deploy: go run ./cmd/factory serve
