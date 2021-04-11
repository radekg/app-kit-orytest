TEST_TIMEOUT ?=120s
CURRENT_DIR  = $(dir $(realpath $(firstword $(MAKEFILE_LIST))))
VERSION      ?= $(shell git describe --tags --always)

.PHONY: coverage
coverage:
	mkdir -p ${CURRENT_DIR}/.coverage
	go test -timeout ${TEST_TIMEOUT} -coverpkg=./...  -covermode=atomic -coverprofile=${CURRENT_DIR}/.coverage/cov.out -v ./...
	go tool cover -html=${CURRENT_DIR}/.coverage/cov.out \
		-o ${CURRENT_DIR}/.coverage/cov.html

.PHONY: commit-coverage
commit-coverage:
	cd ${CURRENT_DIR} && git add .coverage/ && git commit -m "release coverage report" && git push origin master

.PHONY: release
release: coverage commit-coverage
	curl -sL https://raw.githubusercontent.com/radekg/git-release/master/git-release --output /tmp/git-release
	chmod +x /tmp/git-release
	/tmp/git-release --repository-path=${CURRENT_DIR}
	rm -rf /tmp/git-release

.PHONY: test
test:
	go clean -testcache
	go test -timeout ${TEST_TIMEOUT} -cover -v ./...
