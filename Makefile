go_ver != sed -n '/^go\s\+/s/go\s\+\(.*\)/\1/p' go.mod

build:
	go build


debian-bullseye:
	docker create \
		--name docklogs-debian-bullseye-build \
		-w "/build" \
		golang:$(go_ver)-bullseye \
		make build
	for f in $$(find -name "*.go") go.mod go.sum .git Makefile; do \
		docker cp "$$f" docklogs-debian-bullseye-build:/build/; \
	done
	docker start -a docklogs-debian-bullseye-build
	docker cp \
		docklogs-debian-bullseye-build:/build/docklogs \
		./docklogs-debian-bullseye
	docker rm docklogs-debian-bullseye-build


clean:
	-rm -f docklogs
	-rm -f docklogs-debian-bullseye
	-docker rm docklogs-debian-bullseye-build
