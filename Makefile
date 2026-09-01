BINDIR := bin
GO     ?= go

.PHONY: all build test race bench cover fmt vet lint clean shell serve

all: build

build:
	$(GO) build -o $(BINDIR)/ ./cmd/...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

bench:
	$(GO) test -bench=. -benchmem -run=XXX ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint: fmt vet

shell: build
	$(BINDIR)/quill quill.db

serve: build
	$(BINDIR)/quill -serve :15433 quill.db

clean:
	rm -rf $(BINDIR) coverage.out coverage.html *.db *.db.wal
