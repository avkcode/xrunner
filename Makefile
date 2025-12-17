BINS=api worker xrunner xrunner-remote xrunner-tui
LOC_EXTS?=go proto ts tsx js jsx sh bash zsh yaml yml json toml
PREFIX?=/usr/local
BINDIR?=$(PREFIX)/bin
INSTALL?=install

.PHONY: all
all: build

.PHONY: build
build: build-go build-node

.PHONY: build-go
build-go:
	@echo "Building Go binaries"
	@mkdir -p bin
	@rm -f bin/xrunner bin/xrunner-*
	@for target in $(BINS); do \
		echo " go build ./cmd/$$target"; \
		out="$$target"; \
		case "$$target" in \
			api|worker) out="xrunner-$$target" ;; \
			xrunner) out="xrunner" ;; \
		esac; \
		GOOS=$${GOOS:-$(shell go env GOOS)} GOARCH=$${GOARCH:-$(shell go env GOARCH)} go build -o "bin/$$out" "./cmd/$$target"; \
	done

.PHONY: build-node
build-node:
	@echo "Building TypeScript SDK"
	npm run build --if-present

.PHONY: install
install: build-go
	@echo "Installing binaries into $(DESTDIR)$(BINDIR)"
	@$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	@for target in $(BINS); do \
		out="$$target"; \
		case "$$target" in \
			api|worker) out="xrunner-$$target" ;; \
			xrunner) out="xrunner" ;; \
		esac; \
		$(INSTALL) -m 0755 "bin/$$out" "$(DESTDIR)$(BINDIR)/$$out"; \
	done

.PHONY: install-user
install-user:
	@$(MAKE) install PREFIX=$$HOME/.local

.PHONY: uninstall
uninstall:
	@echo "Removing binaries from $(DESTDIR)$(BINDIR)"
	@for target in $(BINS); do \
		out="$$target"; \
		case "$$target" in \
			api|worker) out="xrunner-$$target" ;; \
			xrunner) out="xrunner" ;; \
		esac; \
		rm -f "$(DESTDIR)$(BINDIR)/$$out"; \
	done

.PHONY: clean
clean:
	rm -rf bin

.PHONY: test
test:
	go test ./...

.PHONY: ssh-chat
ssh-chat:
	./scripts/ssh_chat_local.zsh

.PHONY: loc
loc:
	@set -eu; \
	tmp=$$(mktemp); \
	if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
		git ls-files -co --exclude-standard > $$tmp; \
	else \
		find . -type f \( -path './.git/*' -o -path './node_modules/*' -o -path './bin/*' -o -path './build/*' -o -path './vendor/*' \) -prune -o -print > $$tmp; \
	fi; \
	total=0; files=0; \
	while IFS= read -r f; do \
		[ -f "$$f" ] || continue; \
		ext=$${f##*.}; \
		case " $(LOC_EXTS) " in \
			*" $$ext "*) ;; \
			*) continue ;; \
		esac; \
		n=$$(wc -l < "$$f" | tr -d ' '); \
		total=$$((total + n)); \
		files=$$((files + 1)); \
	done < $$tmp; \
	rm -f $$tmp; \
	echo "$$files files, $$total lines (extensions: $(LOC_EXTS))"
