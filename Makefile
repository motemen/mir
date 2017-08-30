ifeq ($(V),)
    VERBOSE_FLAG=
else
    VERBOSE_FLAG=-v
endif

test:
	go test $(VERBOSE_FLAG) ./...

dist:
	gox \
	    -os='linux' \
	    -arch='386 amd64' \
	    -output='build/{{.Dir}}_{{.OS}}_{{.Arch}}'

release:
	git describe --tags --exact-match
	ghr -draft $$(git describe --tags --exact-match) build/
