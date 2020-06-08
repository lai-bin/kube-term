all: build

build:
	`go env GOPATH`/bin/rice embed-go
	go fmt
	go build -o kube-term
